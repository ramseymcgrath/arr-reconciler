// Package llmobs emits Datadog LLM Observability spans and evaluations for the
// reconciler's Claude calls. There is no Go SDK for LLM Observability, so this
// posts directly to a local Datadog agent's EVP proxy:
//
//	spans: POST {endpoint}/api/intake/llm-obs/v1/trace/spans
//	evals: POST {endpoint}/api/intake/llm-obs/v2/eval-metric
//
// In agent mode no API key is required — the agent injects it. A Tracer built
// from an empty endpoint is nil, and every method is nil-safe, so callers can
// instrument unconditionally and it becomes a no-op when disabled.
package llmobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The Datadog agent forwards LLM Observability payloads through its EVP proxy
// (/evp_proxy/v2/), NOT the bare intake paths (which return 404 on the agent).
// Each requires an X-Datadog-EVP-Subdomain header naming the upstream intake.
const (
	spansPath = "/evp_proxy/v2/api/v2/llmobs"
	evalPath  = "/evp_proxy/v2/api/intake/llm-obs/v2/eval-metric"

	spansSubdomain = "llmobs-intake"
	evalSubdomain  = "api"

	evpSubdomainHeader = "X-Datadog-EVP-Subdomain"

	rootParentID = "undefined" // required literal for a root span's parent_id
)

// Tracer posts spans and evaluations to a Datadog agent's LLM Obs intake.
type Tracer struct {
	endpoint string
	mlApp    string
	tags     []string
	http     *http.Client
}

// New returns a Tracer, or nil if endpoint is empty (disabling instrumentation).
// All Tracer/Trace/Span methods are nil-safe, so a nil Tracer is a complete
// no-op.
func New(endpoint, mlApp string, tags []string, timeout time.Duration) *Tracer {
	if endpoint == "" {
		return nil
	}
	return &Tracer{
		endpoint: strings.TrimRight(endpoint, "/"),
		mlApp:    mlApp,
		tags:     tags,
		http:     &http.Client{Timeout: timeout},
	}
}

// Trace accumulates the spans and evaluations for a single reconciler run and
// flushes them together at the end.
type Trace struct {
	tracer   *Tracer
	traceID  string
	rootName string
	root     *Span
	spans    []wireSpan
	evals    []wireMetric
}

// Span is a handle to an open span; pass it as the parent of child spans and to
// Eval to attach evaluations.
type Span struct {
	id      string
	started time.Time
}

// ID returns the span's decimal ID, or "" for a nil span.
func (s *Span) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// StartRun opens a trace with a root "agent" span named name. It returns a nil
// *Trace when the Tracer is disabled.
func (t *Tracer) StartRun(name string) *Trace {
	if t == nil {
		return nil
	}
	tr := &Trace{tracer: t, traceID: randID(), rootName: name}
	tr.root = tr.Start()
	return tr
}

// Root returns the trace's root span (nil when disabled).
func (tr *Trace) Root() *Span {
	if tr == nil {
		return nil
	}
	return tr.root
}

// Start opens a span and stamps its start time. The span's name, kind, and
// parent are supplied later at Finish, which is what actually records it. It
// returns a nil *Span when the trace is disabled.
func (tr *Trace) Start() *Span {
	if tr == nil {
		return nil
	}
	return &Span{id: randID(), started: time.Now()}
}

// FinishOpts carries the content recorded when a span closes.
type FinishOpts struct {
	Kind         string // span kind; defaults to "llm"
	Input        string // input value (meta.input.value)
	Output       string // output value (meta.output.value)
	Model        string // meta.model_name (LLM spans)
	Provider     string // meta.model_provider (LLM spans)
	InputTokens  int    // metrics.input_tokens
	OutputTokens int    // metrics.output_tokens
	Err          error  // sets status=error and meta.error
	Tags         []string
}

// Finish closes span and records it into the trace. Parent linkage is supplied
// explicitly so the engine controls the tree shape.
func (tr *Trace) Finish(span, parent *Span, name string, opt FinishOpts) {
	if tr == nil || span == nil {
		return
	}
	kind := opt.Kind
	if kind == "" {
		kind = "llm"
	}
	parentID := rootParentID
	if parent != nil {
		parentID = parent.id
	}

	meta := wireMeta{Kind: kind}
	if opt.Input != "" {
		meta.Input = &wireIO{Value: opt.Input}
	}
	if opt.Output != "" {
		meta.Output = &wireIO{Value: opt.Output}
	}
	meta.ModelName = opt.Model
	meta.ModelProvider = opt.Provider

	status := ""
	if opt.Err != nil {
		status = "error"
		meta.Error = &wireError{Message: opt.Err.Error(), Type: "error"}
	}

	var metrics map[string]float64
	if opt.InputTokens > 0 || opt.OutputTokens > 0 {
		metrics = map[string]float64{
			"input_tokens":  float64(opt.InputTokens),
			"output_tokens": float64(opt.OutputTokens),
			"total_tokens":  float64(opt.InputTokens + opt.OutputTokens),
		}
	}

	tr.spans = append(tr.spans, wireSpan{
		ParentID: parentID,
		TraceID:  tr.traceID,
		SpanID:   span.id,
		Name:     name,
		Meta:     meta,
		StartNS:  uint64(span.started.UnixNano()),
		Duration: float64(time.Since(span.started).Nanoseconds()),
		Status:   status,
		Metrics:  metrics,
		Tags:     opt.Tags,
	})
}

// Eval is a single evaluation to attach to a span.
type Eval struct {
	// Label is the evaluation name. It is sanitized to [A-Za-z][A-Za-z0-9_]*.
	Label string
	// Score, when non-nil, submits a numeric "score" evaluation.
	Score *float64
	// Categorical, when non-empty, submits a "categorical" evaluation. Exactly
	// one of Score/Categorical should be set.
	Categorical string
	// Assessment is an optional "pass"/"fail" annotation (score metrics).
	Assessment string
	// Reasoning is an optional human/LLM justification.
	Reasoning string
}

// Eval attaches an evaluation to span (joined by span_id + trace_id). It is a
// no-op for a nil trace or span, or an Eval with neither value set.
func (tr *Trace) Eval(span *Span, ev Eval) {
	if tr == nil || span == nil {
		return
	}
	m := wireMetric{
		EvalScope:   "span",
		JoinOn:      joinOn{Span: joinSpan{SpanID: span.id, TraceID: tr.traceID}},
		MLApp:       tr.tracer.mlApp,
		TimestampMS: time.Now().UnixMilli(),
		Label:       sanitizeLabel(ev.Label),
		Assessment:  ev.Assessment,
		Reasoning:   ev.Reasoning,
	}
	switch {
	case ev.Score != nil:
		m.MetricType = "score"
		m.ScoreValue = ev.Score
	case ev.Categorical != "":
		m.MetricType = "categorical"
		m.CategoricalValue = ev.Categorical
	default:
		return // nothing to submit
	}
	tr.evals = append(tr.evals, m)
}

// Flush closes the root span and posts all accumulated spans and evaluations.
// Spans go first so the evaluations have a span to join against. It is a no-op
// for a nil trace.
func (tr *Trace) Flush(ctx context.Context) error {
	if tr == nil {
		return nil
	}
	// Close the root span if it was never explicitly finished.
	if tr.root != nil && !rootRecorded(tr) {
		tr.Finish(tr.root, nil, tr.rootName, FinishOpts{Kind: "agent"})
	}
	if len(tr.spans) > 0 {
		if err := tr.tracer.post(ctx, spansPath, spansSubdomain, spansRequest{
			Data: spansData{
				Type: "span",
				Attributes: spansAttrs{
					MLApp: tr.tracer.mlApp,
					Tags:  tr.tracer.tags,
					Spans: tr.spans,
				},
			},
		}); err != nil {
			return fmt.Errorf("post spans: %w", err)
		}
	}
	if len(tr.evals) > 0 {
		if err := tr.tracer.post(ctx, evalPath, evalSubdomain, evalRequest{
			Data: evalData{
				Type:       "evaluation_metric",
				Attributes: evalAttrs{Metrics: tr.evals},
			},
		}); err != nil {
			return fmt.Errorf("post evals: %w", err)
		}
	}
	return nil
}

// rootRecorded reports whether the root span has already been appended.
func rootRecorded(tr *Trace) bool {
	for i := range tr.spans {
		if tr.spans[i].SpanID == tr.root.id {
			return true
		}
	}
	return false
}

func (t *Tracer) post(ctx context.Context, path, subdomain string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(evpSubdomainHeader, subdomain)

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: status %d: %s", path, resp.StatusCode, string(body))
	}
	return nil
}

// randID returns a 64-bit unsigned integer formatted as a decimal string, the
// format Datadog requires for span_id/trace_id.
func randID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should not fail; fall back to a time-derived value.
		return strconv.FormatUint(uint64(time.Now().UnixNano()), 10)
	}
	return strconv.FormatUint(binary.BigEndian.Uint64(b[:]), 10)
}

// sanitizeLabel coerces s into a valid Datadog evaluation label: it must start
// with an ASCII letter and contain only ASCII alphanumerics or underscores.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "eval"
	}
	return out
}

// ---- wire types (match the LLM Obs HTTP API schema) -----------------------

type spansRequest struct {
	Data spansData `json:"data"`
}

type spansData struct {
	Type       string     `json:"type"`
	Attributes spansAttrs `json:"attributes"`
}

type spansAttrs struct {
	MLApp string     `json:"ml_app"`
	Tags  []string   `json:"tags,omitempty"`
	Spans []wireSpan `json:"spans"`
}

type wireSpan struct {
	ParentID string             `json:"parent_id"`
	TraceID  string             `json:"trace_id"`
	SpanID   string             `json:"span_id"`
	Name     string             `json:"name"`
	Meta     wireMeta           `json:"meta"`
	StartNS  uint64             `json:"start_ns"`
	Duration float64            `json:"duration"`
	Status   string             `json:"status,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	Tags     []string           `json:"tags,omitempty"`
}

type wireMeta struct {
	Kind          string     `json:"kind"`
	Input         *wireIO    `json:"input,omitempty"`
	Output        *wireIO    `json:"output,omitempty"`
	ModelName     string     `json:"model_name,omitempty"`
	ModelProvider string     `json:"model_provider,omitempty"`
	Error         *wireError `json:"error,omitempty"`
}

type wireIO struct {
	Value string `json:"value,omitempty"`
}

type wireError struct {
	Message string `json:"message,omitempty"`
	Type    string `json:"type,omitempty"`
}

type evalRequest struct {
	Data evalData `json:"data"`
}

type evalData struct {
	Type       string    `json:"type"`
	Attributes evalAttrs `json:"attributes"`
}

type evalAttrs struct {
	Metrics []wireMetric `json:"metrics"`
}

type wireMetric struct {
	EvalScope        string   `json:"eval_scope,omitempty"`
	JoinOn           joinOn   `json:"join_on"`
	MLApp            string   `json:"ml_app"`
	TimestampMS      int64    `json:"timestamp_ms"`
	MetricType       string   `json:"metric_type"`
	Label            string   `json:"label"`
	CategoricalValue string   `json:"categorical_value,omitempty"`
	ScoreValue       *float64 `json:"score_value,omitempty"`
	Assessment       string   `json:"assessment,omitempty"`
	Reasoning        string   `json:"reasoning,omitempty"`
}

type joinOn struct {
	Span joinSpan `json:"span"`
}

type joinSpan struct {
	SpanID  string `json:"span_id"`
	TraceID string `json:"trace_id"`
}
