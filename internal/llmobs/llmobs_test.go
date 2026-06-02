package llmobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestDisabledTracerIsNoop(t *testing.T) {
	var tr *Tracer = New("", "app", nil, time.Second)
	if tr != nil {
		t.Fatal("expected nil tracer for empty endpoint")
	}
	// All methods must be safe on the nil chain.
	trace := tr.StartRun("run")
	span := trace.Start()
	trace.Finish(span, trace.Root(), "x", FinishOpts{})
	score := 0.5
	trace.Eval(span, Eval{Label: "x", Score: &score})
	if err := trace.Flush(context.Background()); err != nil {
		t.Fatalf("flush on disabled tracer: %v", err)
	}
}

// capture is a fake Datadog agent that records the JSON bodies it receives.
type capture struct {
	mu    sync.Mutex
	spans spansRequest
	evals evalRequest
	paths []string
}

func newAgent(t *testing.T, cap *capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		defer cap.mu.Unlock()
		cap.paths = append(cap.paths, r.URL.Path)
		sub := r.Header.Get(evpSubdomainHeader)
		switch r.URL.Path {
		case spansPath:
			if sub != spansSubdomain {
				t.Errorf("spans subdomain header = %q, want %q", sub, spansSubdomain)
			}
			if err := json.Unmarshal(body, &cap.spans); err != nil {
				t.Errorf("bad spans body: %v", err)
			}
		case evalPath:
			if sub != evalSubdomain {
				t.Errorf("eval subdomain header = %q, want %q", sub, evalSubdomain)
			}
			if err := json.Unmarshal(body, &cap.evals); err != nil {
				t.Errorf("bad evals body: %v", err)
			}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
}

func TestFullTraceSchema(t *testing.T) {
	cap := &capture{}
	srv := newAgent(t, cap)
	defer srv.Close()

	tr := New(srv.URL, "arr-reconciler", []string{"env:home"}, time.Second)
	trace := tr.StartRun("reconcile_run")

	// An llm span under the root.
	llm := trace.Start()
	trace.Finish(llm, trace.Root(), "queue_triage:sonarr", FinishOpts{
		Kind: "llm", Input: "[candidates]", Output: "[decisions]",
		Model: "claude-opus-4-8", Provider: "anthropic",
		InputTokens: 100, OutputTokens: 20,
	})

	// A task span (decision) under the llm span, with both evals.
	task := trace.Start()
	trace.Finish(task, llm, "decision:q-1", FinishOpts{Kind: "task", Input: "Some.Movie"})
	conf := 0.9
	trace.Eval(task, Eval{Label: "model_confidence", Score: &conf})
	trace.Eval(task, Eval{Label: "rail_outcome", Categorical: "executed"})

	if err := trace.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()

	// Spans posted before evals.
	if len(cap.paths) != 2 || cap.paths[0] != spansPath || cap.paths[1] != evalPath {
		t.Fatalf("unexpected request order: %v", cap.paths)
	}

	sp := cap.spans.Data.Attributes
	if cap.spans.Data.Type != "span" {
		t.Errorf("span data type = %q", cap.spans.Data.Type)
	}
	if sp.MLApp != "arr-reconciler" {
		t.Errorf("ml_app = %q", sp.MLApp)
	}
	// root + llm + task = 3 spans.
	if len(sp.Spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(sp.Spans))
	}

	byName := map[string]wireSpan{}
	for _, s := range sp.Spans {
		byName[s.Name] = s
		// IDs must be decimal strings.
		if _, err := strconv.ParseUint(s.SpanID, 10, 64); err != nil {
			t.Errorf("span_id %q not decimal", s.SpanID)
		}
		if _, err := strconv.ParseUint(s.TraceID, 10, 64); err != nil {
			t.Errorf("trace_id %q not decimal", s.TraceID)
		}
		if s.StartNS == 0 || s.Duration < 0 {
			t.Errorf("span %q bad timing start=%d dur=%v", s.Name, s.StartNS, s.Duration)
		}
	}

	root := byName["reconcile_run"]
	if root.ParentID != rootParentID {
		t.Errorf("root parent_id = %q, want %q", root.ParentID, rootParentID)
	}
	if root.Meta.Kind != "agent" {
		t.Errorf("root kind = %q, want agent", root.Meta.Kind)
	}

	llmSpan := byName["queue_triage:sonarr"]
	if llmSpan.ParentID != root.SpanID {
		t.Error("llm span not parented to root")
	}
	if llmSpan.Meta.Kind != "llm" || llmSpan.Meta.ModelName != "claude-opus-4-8" {
		t.Errorf("llm meta wrong: %+v", llmSpan.Meta)
	}
	if llmSpan.Metrics["total_tokens"] != 120 {
		t.Errorf("total_tokens = %v, want 120", llmSpan.Metrics["total_tokens"])
	}

	taskSpan := byName["decision:q-1"]
	if taskSpan.ParentID != llmSpan.SpanID {
		t.Error("task span not parented to llm span")
	}

	// Evals: one score + one categorical, joined on the task span.
	ms := cap.evals.Data.Attributes.Metrics
	if cap.evals.Data.Type != "evaluation_metric" {
		t.Errorf("eval data type = %q", cap.evals.Data.Type)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d evals, want 2", len(ms))
	}
	for _, m := range ms {
		if m.JoinOn.Span.SpanID != taskSpan.SpanID || m.JoinOn.Span.TraceID != taskSpan.TraceID {
			t.Errorf("eval %q join_on mismatch", m.Label)
		}
		if m.MLApp != "arr-reconciler" || m.TimestampMS == 0 {
			t.Errorf("eval %q missing ml_app/timestamp", m.Label)
		}
		switch m.Label {
		case "model_confidence":
			if m.MetricType != "score" || m.ScoreValue == nil || *m.ScoreValue != 0.9 {
				t.Errorf("confidence eval wrong: %+v", m)
			}
		case "rail_outcome":
			if m.MetricType != "categorical" || m.CategoricalValue != "executed" {
				t.Errorf("rail_outcome eval wrong: %+v", m)
			}
		default:
			t.Errorf("unexpected eval label %q", m.Label)
		}
	}
}

func TestErrorSpanSetsStatus(t *testing.T) {
	cap := &capture{}
	srv := newAgent(t, cap)
	defer srv.Close()

	tr := New(srv.URL, "app", nil, time.Second)
	trace := tr.StartRun("run")
	s := trace.Start()
	trace.Finish(s, trace.Root(), "failing", FinishOpts{Kind: "llm", Err: io.ErrUnexpectedEOF})
	if err := trace.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	var found bool
	for _, s := range cap.spans.Data.Attributes.Spans {
		if s.Name == "failing" {
			found = true
			if s.Status != "error" || s.Meta.Error == nil {
				t.Errorf("error span not marked: %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("failing span not posted")
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"model_confidence": "model_confidence",
		"rail outcome":     "rail_outcome",
		"123start":         "_123start",
		"weird!@#chars":    "weird___chars",
		"":                 "eval",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRandIDIsDecimalAndVaries(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := randID()
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			t.Fatalf("id %q not a decimal uint64", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
