// Package local is a cheap, high-volume prefilter tier backed by a local Ollama
// model. It classifies candidates as keep / junk / uncertain so the engine can
// finalize only the SAFE direction (keep) locally and escalate everything else
// (junk + uncertain) to the frontier model (Claude).
//
// Safety posture (load-bearing): this model never finalizes a destructive
// action. It may only drop high-confidence, non-destructive "keep" items from
// the expensive prompt. Anything it calls junk, is unsure about, or that fails
// to parse becomes Escalate — fail-open toward the frontier model. A Classifier
// built from an empty endpoint is nil, and every method is nil-safe, so the
// caller can prefilter unconditionally and it becomes a transparent pass-through
// (everything escalates) when disabled.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Verdict is the prefilter's label for a single candidate.
type Verdict string

const (
	// Keep means the file/item is clearly fine to leave alone. This is the only
	// verdict the engine may act on without consulting the frontier model,
	// because acting on it means doing nothing destructive.
	Keep Verdict = "keep"
	// Junk means the model thinks it is removable. The engine does NOT trust
	// this directly — it escalates to the frontier model for the real decision.
	Junk Verdict = "junk"
	// Uncertain means the model could not decide; always escalates.
	Uncertain Verdict = "uncertain"
)

// Escalate reports whether a verdict must go to the frontier model. Everything
// except a confident Keep escalates.
func (v Verdict) Escalate() bool { return v != Keep }

// Classifier calls a local Ollama model's /api/chat endpoint with a strict
// JSON-Schema response format.
type Classifier struct {
	endpoint string
	model    string
	http     *http.Client
}

// New returns a Classifier, or nil if endpoint is empty (disabling the tier).
// All methods are nil-safe.
func New(endpoint, model string, timeout time.Duration) *Classifier {
	if endpoint == "" {
		return nil
	}
	return &Classifier{
		endpoint: strings.TrimRight(endpoint, "/"),
		model:    model,
		http:     &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether the prefilter tier is active.
func (c *Classifier) Enabled() bool { return c != nil }

// Preload asks Ollama to load the model into memory (empty message list) and
// keep it resident, so the first real classification isn't slowed by a cold
// start. It is best-effort: errors are returned for logging but are not fatal.
func (c *Classifier) Preload(ctx context.Context) error {
	if c == nil {
		return nil
	}
	body := chatRequest{Model: c.model, Messages: []chatMessage{}, KeepAlive: -1, Stream: false}
	return c.post(ctx, body, nil)
}

// Item is one candidate to classify: an opaque Ref and a compact Text the model
// reads (e.g. a path + size/age summary).
type Item struct {
	Ref  string
	Text string
}

const systemPrompt = `You are a conservative prefilter for a media-library cleanup tool.
For each candidate you are given, output a label:
- "keep": the item is clearly something to LEAVE ALONE (a real media file, a subtitle/artwork/nfo sidecar of a tracked item, anything you are not confident is junk).
- "junk": the item is very obviously removable leftover (e.g. a zero-byte stray, an orphaned .metathumb/.xml/sample with no real media).
- "uncertain": you cannot tell.

Bias strongly toward "keep" and "uncertain". Only say "junk" when it is obvious. A human-reviewed stronger model re-checks everything that is not "keep", so over-escalating is cheap and safe, but wrongly labeling a real file "keep"... is fine (it just won't be cleaned). Never invent items. Respond ONLY with JSON matching the schema.`

// Classify labels each item. The returned map is keyed by Item.Ref. Any item the
// model omits, or any transport/parse failure, yields Uncertain for the affected
// items so the caller escalates them (fail-open). A nil Classifier returns all
// Uncertain, making the whole tier a transparent pass-through.
func (c *Classifier) Classify(ctx context.Context, items []Item) map[string]Verdict {
	out := make(map[string]Verdict, len(items))
	// Default everything to Uncertain first; we only downgrade to Keep/Junk on a
	// clean, validated response. This guarantees fail-open behavior on any error.
	for _, it := range items {
		out[it.Ref] = Uncertain
	}
	if c == nil || len(items) == 0 {
		return out
	}

	var sb strings.Builder
	sb.WriteString("Classify these candidates:\n")
	for _, it := range items {
		fmt.Fprintf(&sb, "- id=%s: %s\n", it.Ref, it.Text)
	}

	req := chatRequest{
		Model:     c.model,
		Stream:    false,
		KeepAlive: -1,
		Format:    classifySchema,
		Options:   chatOptions{Temperature: 0, Seed: 42, NumCtx: 8192},
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: sb.String()},
		},
	}

	var resp chatResponse
	if err := c.post(ctx, req, &resp); err != nil {
		return out // all Uncertain -> all escalate
	}

	var parsed classifyResult
	if err := json.Unmarshal([]byte(resp.Message.Content), &parsed); err != nil {
		return out
	}
	for _, r := range parsed.Results {
		if _, ok := out[r.ID]; !ok {
			continue // model invented an id; ignore
		}
		switch Verdict(r.Label) {
		case Keep:
			out[r.ID] = Keep
		case Junk:
			out[r.ID] = Junk
		default:
			out[r.ID] = Uncertain
		}
	}
	return out
}

func (c *Classifier) post(ctx context.Context, body any, out *chatResponse) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post /api/chat: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("/api/chat status %d: %s", resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// ---- wire types -----------------------------------------------------------

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	KeepAlive int           `json:"keep_alive"`
	Format    any           `json:"format,omitempty"`
	Options   chatOptions   `json:"options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatOptions struct {
	Temperature float64 `json:"temperature"`
	Seed        int     `json:"seed"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

type chatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

type classifyResult struct {
	Results []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"results"`
}

// classifySchema is the JSON Schema passed as the Ollama "format" so the model
// is structurally constrained to our three labels (enum) per result.
var classifySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"results": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    map[string]any{"type": "string"},
					"label": map[string]any{"type": "string", "enum": []string{"keep", "junk", "uncertain"}},
				},
				"required": []string{"id", "label"},
			},
		},
	},
	"required": []string{"results"},
}
