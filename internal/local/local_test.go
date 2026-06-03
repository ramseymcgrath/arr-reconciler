package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerdictEscalate(t *testing.T) {
	if Keep.Escalate() {
		t.Error("keep must NOT escalate")
	}
	if !Junk.Escalate() || !Uncertain.Escalate() {
		t.Error("junk and uncertain must escalate")
	}
}

func TestDisabledClassifierIsPassThrough(t *testing.T) {
	var c *Classifier = New("", "m", time.Second)
	if c.Enabled() {
		t.Fatal("empty endpoint should disable")
	}
	got := c.Classify(context.Background(), []Item{{Ref: "a"}, {Ref: "b"}})
	for ref, v := range got {
		if v != Uncertain {
			t.Errorf("disabled classifier must return Uncertain (escalate) for %s, got %s", ref, v)
		}
	}
	if err := c.Preload(context.Background()); err != nil {
		t.Errorf("preload on nil classifier: %v", err)
	}
}

func mockOllama(t *testing.T, results map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var arr []map[string]string
		for id, label := range results {
			arr = append(arr, map[string]string{"id": id, "label": label})
		}
		content, _ := json.Marshal(map[string]any{"results": arr})
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"content": string(content)},
		})
	}))
}

func TestClassifyMapsLabels(t *testing.T) {
	srv := mockOllama(t, map[string]string{"a": "keep", "b": "junk", "c": "uncertain"})
	defer srv.Close()

	c := New(srv.URL, "qwen2.5:3b", time.Second)
	got := c.Classify(context.Background(), []Item{{Ref: "a"}, {Ref: "b"}, {Ref: "c"}})
	if got["a"] != Keep || got["b"] != Junk || got["c"] != Uncertain {
		t.Fatalf("bad mapping: %+v", got)
	}
}

// An item the model omits from its response must default to Uncertain (escalate),
// never silently disappear.
func TestClassifyOmittedItemEscalates(t *testing.T) {
	srv := mockOllama(t, map[string]string{"a": "keep"}) // omits "b"
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	got := c.Classify(context.Background(), []Item{{Ref: "a"}, {Ref: "b"}})
	if got["a"] != Keep {
		t.Errorf("a should be keep, got %s", got["a"])
	}
	if got["b"] != Uncertain {
		t.Errorf("omitted b must default to Uncertain, got %s", got["b"])
	}
}

// Any transport/HTTP failure must fail open: every item Uncertain (escalate).
func TestClassifyServerErrorFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	got := c.Classify(context.Background(), []Item{{Ref: "a"}, {Ref: "b"}})
	for ref, v := range got {
		if v != Uncertain {
			t.Errorf("server error must fail open to Uncertain for %s, got %s", ref, v)
		}
	}
}

// A garbage/unparseable model response must also fail open.
func TestClassifyBadJSONFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": "not json at all"}})
	}))
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	got := c.Classify(context.Background(), []Item{{Ref: "a"}})
	if got["a"] != Uncertain {
		t.Errorf("unparseable response must fail open, got %s", got["a"])
	}
}

// An unexpected label value is treated as Uncertain, not acted on.
func TestClassifyUnknownLabelIsUncertain(t *testing.T) {
	srv := mockOllama(t, map[string]string{"a": "delete-everything"})
	defer srv.Close()

	c := New(srv.URL, "m", time.Second)
	got := c.Classify(context.Background(), []Item{{Ref: "a"}})
	if got["a"] != Uncertain {
		t.Errorf("unknown label must be Uncertain, got %s", got["a"])
	}
}

// Many items must be split into multiple /api/chat calls, and a single failing
// batch must only leave its own items Uncertain (the rest still classify).
func TestClassifyBatchesAndIsolatesFailures(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		// Fail the 2nd batch only.
		if calls == 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// Echo back every id seen in this batch as "keep".
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.Unmarshal(body, &req)
		var results []map[string]string
		for _, line := range splitLines(req.Messages[len(req.Messages)-1].Content) {
			if id := idFromLine(line); id != "" {
				results = append(results, map[string]string{"id": id, "label": "keep"})
			}
		}
		content, _ := json.Marshal(map[string]any{"results": results})
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(content)}})
	}))
	defer srv.Close()

	c := New(srv.URL, "m", 5*time.Second)
	// 45 items -> 3 batches of 20/20/5.
	var items []Item
	for i := 0; i < 45; i++ {
		items = append(items, Item{Ref: fmt.Sprintf("o%d", i)})
	}
	got := c.Classify(context.Background(), items)

	if calls != 3 {
		t.Errorf("expected 3 batches, got %d calls", calls)
	}
	// Batch 1 (o0-o19) = keep, batch 2 (o20-o39) failed = Uncertain, batch 3 (o40-o44) = keep.
	if got["o0"] != Keep || got["o19"] != Keep {
		t.Error("batch 1 should be keep")
	}
	if got["o25"] != Uncertain {
		t.Errorf("failed batch 2 must stay Uncertain, got %s", got["o25"])
	}
	if got["o44"] != Keep {
		t.Errorf("batch 3 should be keep, got %s", got["o44"])
	}
}

func splitLines(s string) []string {
	var out, cur = []string{}, ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return append(out, cur)
}

func idFromLine(line string) string {
	// line like "- id=o5: ..."
	i := indexOf(line, "id=")
	if i < 0 {
		return ""
	}
	rest := line[i+3:]
	j := indexOf(rest, ":")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
