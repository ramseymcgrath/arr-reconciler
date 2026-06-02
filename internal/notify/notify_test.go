package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendEmptyURLIsNoop(t *testing.T) {
	n := New("", time.Second)
	if err := n.Send(context.Background(), "hello"); err != nil {
		t.Fatalf("expected nil for empty url, got %v", err)
	}
}

func TestSendPostsBothKeys(t *testing.T) {
	var gotContent, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		gotContent = m["content"]
		gotText = m["text"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := New(srv.URL, time.Second)
	if err := n.Send(context.Background(), "run done"); err != nil {
		t.Fatal(err)
	}
	if gotContent != "run done" || gotText != "run done" {
		t.Fatalf("content=%q text=%q", gotContent, gotText)
	}
}

func TestSendTruncatesToDiscordLimit(t *testing.T) {
	var gotLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		gotLen = len(m["content"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(srv.URL, time.Second)
	if err := n.Send(context.Background(), strings.Repeat("x", 5000)); err != nil {
		t.Fatal(err)
	}
	if gotLen > discordContentLimit {
		t.Fatalf("content length %d exceeds limit %d", gotLen, discordContentLimit)
	}
}

func TestSendReturnsErrorOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New(srv.URL, time.Second)
	if err := n.Send(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 500 status")
	}
}
