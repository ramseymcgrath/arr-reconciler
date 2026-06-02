package arr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ramseymcgrath/arr-reconciler/internal/config"
)

func testClient(url string) *Client {
	return New(config.Instance{Name: "sonarr", Kind: "sonarr", BaseURL: url, APIKey: "k"}, time.Second)
}

// A 404 on delete means the item is already gone (e.g. a season-pack sibling
// removed when the first record was deleted) — that is the desired end state,
// so DeleteQueueItem must treat it as success.
func TestDeleteQueueItem404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	if err := testClient(srv.URL).DeleteQueueItem(context.Background(), 123, true, true); err != nil {
		t.Fatalf("404 should be treated as success, got: %v", err)
	}
}

func TestDeleteQueueItemSuccess(t *testing.T) {
	var gotMethod, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testClient(srv.URL).DeleteQueueItem(context.Background(), 7, true, true); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	for _, want := range []string{"removeFromClient=true", "blocklist=true"} {
		if !contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

// Other non-2xx statuses must still surface as errors.
func TestDeleteQueueItem500IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := testClient(srv.URL).DeleteQueueItem(context.Background(), 1, true, true); err == nil {
		t.Fatal("expected error on 500")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
