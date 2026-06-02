package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGatewayHeadersSet(t *testing.T) {
	var gotAuth, gotKey, gotTTL, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("cf-aig-authorization")
		gotKey = r.Header.Get("x-api-key")
		gotTTL = r.Header.Get("cf-aig-cache-ttl")
		w.Write([]byte(`{"content":[{"type":"text","text":"[]"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(Options{
		APIKey: "sk-real", Model: "m", MaxTokens: 16, BaseURL: srv.URL,
		GatewayToken: "cf-tok", CacheTTLSeconds: 3600, Timeout: 5 * time.Second,
	})
	if _, err := c.Decide(context.Background(), "", "sys", "[]"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "sk-real" {
		t.Errorf("x-api-key = %q (anthropic key must still be sent)", gotKey)
	}
	if gotAuth != "Bearer cf-tok" {
		t.Errorf("cf-aig-authorization = %q, want 'Bearer cf-tok'", gotAuth)
	}
	if gotTTL != "3600" {
		t.Errorf("cf-aig-cache-ttl = %q, want 3600", gotTTL)
	}
}

func TestNoGatewayHeadersWhenDirect(t *testing.T) {
	var gotAuth, gotTTL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("cf-aig-authorization")
		gotTTL = r.Header.Get("cf-aig-cache-ttl")
		w.Write([]byte(`{"content":[{"type":"text","text":"[]"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	c := New(Options{APIKey: "sk", Model: "m", MaxTokens: 16, BaseURL: srv.URL, Timeout: 5 * time.Second})
	if _, err := c.Decide(context.Background(), "", "sys", "[]"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" || gotTTL != "" {
		t.Errorf("gateway headers leaked when direct: auth=%q ttl=%q", gotAuth, gotTTL)
	}
}

func TestParseDecisionsPlain(t *testing.T) {
	in := `[{"ref":"q-1","action":"remove","reason":"stalled","confidence":0.9}]`
	ds, err := parseDecisions(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Ref != "q-1" || ds[0].Action != "remove" {
		t.Fatalf("bad parse: %+v", ds)
	}
}

func TestParseDecisionsWithFences(t *testing.T) {
	in := "```json\n[{\"ref\":\"o-2\",\"action\":\"keep\",\"reason\":\"sidecar\",\"confidence\":0.2}]\n```"
	ds, err := parseDecisions(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Action != "keep" {
		t.Fatalf("bad parse: %+v", ds)
	}
}

func TestParseDecisionsWithProse(t *testing.T) {
	in := `Sure, here are my decisions:
	[{"ref":"q-3","action":"keep","reason":"progressing","confidence":0.8}]
	Let me know if you need more.`
	ds, err := parseDecisions(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Ref != "q-3" {
		t.Fatalf("bad parse: %+v", ds)
	}
}

func TestParseDecisionsNoArray(t *testing.T) {
	if _, err := parseDecisions("I cannot help with that."); err == nil {
		t.Fatal("expected error when no JSON array present")
	}
}

func TestTruncate(t *testing.T) {
	// truncate keeps n bytes then appends an ellipsis (used for error context).
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := truncate("hello world", 8); got != "hello wo..." {
		t.Errorf("got %q", got)
	}
}
