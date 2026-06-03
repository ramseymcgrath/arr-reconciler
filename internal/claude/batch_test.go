package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Simulates Anthropic's batch lifecycle through the SDK: create -> poll (ends on
// 2nd Get) -> stream JSONL results out of order, with one errored item.
func TestDecideBatchFullFlow(t *testing.T) {
	var polls int
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/v1/messages/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth headers on create")
		}
		writeJSON(w, map[string]any{
			"id": "msgbatch_test", "type": "message_batch",
			"processing_status": "in_progress",
			"request_counts":    map[string]int{"processing": 3, "succeeded": 0, "errored": 0, "canceled": 0, "expired": 0},
		})
	})

	mux.HandleFunc("/v1/messages/batches/msgbatch_test", func(w http.ResponseWriter, r *http.Request) {
		polls++
		status := "in_progress"
		results := any(nil)
		if polls >= 2 {
			status = "ended"
			results = "https://api.anthropic.com/v1/messages/batches/msgbatch_test/results"
		}
		writeJSON(w, map[string]any{
			"id": "msgbatch_test", "type": "message_batch",
			"processing_status": status,
			"results_url":       results,
			"request_counts":    map[string]int{"processing": 0, "succeeded": 2, "errored": 1, "canceled": 0, "expired": 0},
		})
	})

	mux.HandleFunc("/v1/messages/batches/msgbatch_test/results", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-jsonl")
		mk := func(id, text string) string {
			msg := map[string]any{
				"id": "msg_x", "type": "message", "role": "assistant", "model": "claude-haiku-4-5",
				"content":     []map[string]any{{"type": "text", "text": text}},
				"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
				"stop_reason": "end_turn",
			}
			line, _ := json.Marshal(map[string]any{
				"custom_id": id,
				"result":    map[string]any{"type": "succeeded", "message": msg},
			})
			return string(line)
		}
		errLine, _ := json.Marshal(map[string]any{
			"custom_id": "q-3",
			"result":    map[string]any{"type": "errored", "error": map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "bad"}}},
		})
		// Out of order: q-2 before q-1.
		fmt.Fprintln(w, mk("q-2", `[{"ref":"q-2","action":"keep","reason":"ok","confidence":0.3}]`))
		fmt.Fprintln(w, mk("q-1", `[{"ref":"q-1","action":"remove","reason":"dead","confidence":0.9}]`))
		fmt.Fprintln(w, string(errLine))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Options{APIKey: "sk", Model: "claude-haiku-4-5", MaxTokens: 16, BaseURL: srv.URL, Timeout: 5 * time.Second})
	reqs := []BatchRequest{
		{CustomID: "q-1", System: "sys", Payload: "p1"},
		{CustomID: "q-2", System: "sys", Payload: "p2"},
		{CustomID: "q-3", System: "sys", Payload: "p3"},
	}
	results, err := c.DecideBatch(context.Background(), reqs, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	byID := map[string]BatchResult{}
	for _, r := range results {
		byID[r.CustomID] = r
	}
	if r := byID["q-1"]; r.Result == nil || len(r.Result.Decisions) != 1 || r.Result.Decisions[0].Action != "remove" {
		t.Errorf("q-1 wrong: %+v", r)
	}
	if r := byID["q-2"]; r.Result == nil || r.Result.Decisions[0].Action != "keep" {
		t.Errorf("q-2 wrong: %+v", r)
	}
	if r := byID["q-3"]; r.Err == nil || !strings.Contains(r.Err.Error(), "errored") {
		t.Errorf("q-3 should carry an error, got %+v", r)
	}
	if polls < 2 {
		t.Errorf("expected to poll until ended, polls=%d", polls)
	}
}

func TestDecideBatchEmpty(t *testing.T) {
	c := New(Options{APIKey: "sk", Model: "m", Timeout: time.Second})
	res, err := c.DecideBatch(context.Background(), nil, 0)
	if err != nil || res != nil {
		t.Errorf("empty batch should be a no-op, got %v %v", res, err)
	}
}
