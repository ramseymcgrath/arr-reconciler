package claude

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// BatchRequest is one item to submit in a Message Batch. CustomID correlates the
// result back (Anthropic does not guarantee result ordering); it must match
// ^[a-zA-Z0-9_-]{1,64}$ and be unique within the batch.
type BatchRequest struct {
	CustomID string
	Model    string // "" uses the client default
	System   string
	Payload  string // user message content
}

// BatchResult pairs a CustomID with its parsed Result or an error. Exactly one
// of Result/Err is set.
type BatchResult struct {
	CustomID string
	Result   *Result
	Err      error
}

// DecideBatch submits reqs as a single Message Batch (via the official SDK),
// polls until the batch ends, then streams and parses every result. It returns
// one BatchResult per input, keyed by CustomID. Processing is asynchronous on
// Anthropic's side (usually minutes, up to 24h) and billed at 50% of standard
// token prices.
//
// pollInterval bounds how often the batch status is checked; pass 0 for a 30s
// default. The supplied ctx bounds the whole operation (submit + poll + stream),
// so give it a generous deadline.
func (c *Client) DecideBatch(ctx context.Context, reqs []BatchRequest, pollInterval time.Duration) ([]BatchResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}

	// Build the batch. Each request's params mirror a normal Messages call.
	params := make([]anthropic.MessageBatchNewParamsRequest, 0, len(reqs))
	modelOf := make(map[string]string, len(reqs))
	for _, r := range reqs {
		model := r.Model
		if model == "" {
			model = c.model
		}
		modelOf[r.CustomID] = model

		p := anthropic.MessageBatchNewParamsRequestParams{
			Model:     anthropic.Model(model),
			MaxTokens: int64(c.maxTokens),
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(r.Payload)),
			},
		}
		if r.System != "" {
			p.System = []anthropic.TextBlockParam{{Text: r.System}}
		}
		params = append(params, anthropic.MessageBatchNewParamsRequest{
			CustomID: r.CustomID,
			Params:   p,
		})
	}

	batch, err := c.sdk.Messages.Batches.New(ctx, anthropic.MessageBatchNewParams{Requests: params})
	if err != nil {
		return nil, fmt.Errorf("submit batch: %w", err)
	}

	if err := c.waitForBatch(ctx, batch.ID, pollInterval); err != nil {
		return nil, fmt.Errorf("poll batch %s: %w", batch.ID, err)
	}

	return c.streamBatchResults(ctx, batch.ID, reqs, modelOf)
}

// waitForBatch polls until the batch's processing_status is "ended".
func (c *Client) waitForBatch(ctx context.Context, batchID string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		b, err := c.sdk.Messages.Batches.Get(ctx, batchID)
		if err != nil {
			return err
		}
		if b.ProcessingStatus == anthropic.MessageBatchProcessingStatusEnded {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// streamBatchResults streams the JSONL results and maps each line back to its
// request by custom_id (order is not guaranteed). Succeeded results are parsed
// into Decisions; errored/canceled/expired carry an error.
func (c *Client) streamBatchResults(ctx context.Context, batchID string, reqs []BatchRequest, modelOf map[string]string) ([]BatchResult, error) {
	byID := make(map[string]*Result, len(reqs))
	errByID := make(map[string]error, len(reqs))

	stream := c.sdk.Messages.Batches.ResultsStreaming(ctx, batchID)
	for stream.Next() {
		entry := stream.Current()
		switch entry.Result.Type {
		case "succeeded":
			res, perr := resultFromSDKMessage(&entry.Result.Message, modelOf[entry.CustomID])
			if perr != nil {
				errByID[entry.CustomID] = perr
				continue
			}
			byID[entry.CustomID] = res
		default: // errored | canceled | expired
			errByID[entry.CustomID] = fmt.Errorf("batch result %s", entry.Result.Type)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("stream results: %w", err)
	}

	out := make([]BatchResult, 0, len(reqs))
	for _, r := range reqs {
		br := BatchResult{CustomID: r.CustomID}
		if res, ok := byID[r.CustomID]; ok {
			br.Result = res
		} else if e, ok := errByID[r.CustomID]; ok {
			br.Err = e
		} else {
			br.Err = fmt.Errorf("no result returned for %s", r.CustomID)
		}
		out = append(out, br)
	}
	return out, nil
}

// resultFromSDKMessage extracts the text from an SDK Message, parses the decision
// JSON, and bundles token usage — the batch analogue of resultFromMessage.
func resultFromSDKMessage(msg *anthropic.Message, model string) (*Result, error) {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return nil, fmt.Errorf("empty model response")
	}
	decisions, err := parseDecisions(text)
	if err != nil {
		return nil, fmt.Errorf("parse decisions: %w (raw: %q)", err, truncate(text, 500))
	}
	return &Result{
		Decisions: decisions,
		Usage: Usage{
			InputTokens:  int(msg.Usage.InputTokens),
			OutputTokens: int(msg.Usage.OutputTokens),
		},
		Model: model,
	}, nil
}
