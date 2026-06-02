package local

import (
	"context"
	"encoding/json"
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
