package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const anthropicVersion = "2023-06-01"

// Client calls the Anthropic Messages API to make reconciliation decisions.
type Client struct {
	apiKey      string
	model       string
	maxTokens   int
	baseURL     string
	gatewayAuth string // value for cf-aig-authorization (empty = direct to Anthropic)
	cacheTTL    int    // cf-aig-cache-ttl seconds (0 = gateway default / off)
	http        *http.Client
	// sdk is the official Anthropic SDK client, used for the Message Batches API
	// (submit/poll/stream-results). It is configured with the same base URL and
	// gateway headers as the hand-rolled sync path.
	sdk anthropic.Client
}

// Options configures a Client. Only APIKey, Model, and Timeout are strictly
// required; the rest carry sensible zero-value behavior (direct-to-Anthropic).
type Options struct {
	APIKey    string
	Model     string
	MaxTokens int
	// BaseURL is the Messages API base; the client appends "/v1/messages".
	// Point this at a Cloudflare AI Gateway Anthropic-native path to enable
	// gateway features. Defaults to the public Anthropic API.
	BaseURL string
	// GatewayToken, when set, is sent as "cf-aig-authorization: Bearer <token>"
	// so requests authenticate to a Cloudflare AI Gateway. Empty = go direct.
	GatewayToken string
	// CacheTTLSeconds, when > 0, sets "cf-aig-cache-ttl" so the gateway caches
	// identical responses for that long. Only meaningful with a gateway.
	CacheTTLSeconds int
	Timeout         time.Duration
}

// New constructs a Claude client from Options.
func New(o Options) *Client {
	base := o.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	gwAuth := ""
	if o.GatewayToken != "" {
		gwAuth = "Bearer " + o.GatewayToken
	}

	// SDK client for the Batches API: same base URL and auth, plus the gateway
	// header when present. No per-request timeout — batch polls can run long and
	// are bounded by the caller's context instead.
	sdkOpts := []option.RequestOption{
		option.WithAPIKey(o.APIKey),
		option.WithBaseURL(strings.TrimRight(base, "/") + "/"),
		option.WithHTTPClient(&http.Client{}),
	}
	if gwAuth != "" {
		sdkOpts = append(sdkOpts, option.WithHeader("cf-aig-authorization", gwAuth))
	}

	return &Client{
		apiKey:      o.APIKey,
		model:       o.Model,
		maxTokens:   o.MaxTokens,
		baseURL:     strings.TrimRight(base, "/"),
		gatewayAuth: gwAuth,
		cacheTTL:    o.CacheTTLSeconds,
		http:        &http.Client{Timeout: o.Timeout},
		sdk:         anthropic.NewClient(sdkOpts...),
	}
}

type messagesRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Messages  []apiMessage `json:"messages"`
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Usage reports token counts for a single Messages API call, for observability.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Result bundles the parsed decisions with the call's token usage and the model
// that produced them, so callers can record observability metrics.
type Result struct {
	Decisions []Decision
	Usage     Usage
	Model     string
}

// Decision is the structured verdict Claude returns for a single candidate.
type Decision struct {
	// Ref correlates the decision back to the candidate (we pass an opaque id).
	Ref string `json:"ref"`
	// Action is one of: "remove", "keep", "rescan", "trash", "skip".
	Action string `json:"action"`
	// Reason is a short human-readable justification.
	Reason string `json:"reason"`
	// Confidence in [0,1]; the engine may apply a floor.
	Confidence float64 `json:"confidence"`
}

// Decide sends a system prompt plus a JSON payload of candidates and parses the
// model's JSON array of decisions. model selects the model for this call; pass
// "" to use the client's default. The caller is responsible for enforcing
// safety rails on top of whatever the model returns. The returned Result also
// carries token usage and the model name for observability.
func (c *Client) Decide(ctx context.Context, model, system, userPayload string) (*Result, error) {
	if model == "" {
		model = c.model
	}
	reqBody := messagesRequest{
		Model:     model,
		MaxTokens: c.maxTokens,
		System:    system,
		Messages: []apiMessage{
			{Role: "user", Content: userPayload},
		},
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	c.setHeaders(req, true)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call messages api: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("messages api status %d: %s", resp.StatusCode, string(body))
	}

	var mr messagesResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return resultFromMessage(&mr, model)
}

// setHeaders applies the auth/version (and gateway) headers shared by the sync
// and batch paths. Set content for requests with a JSON body.
func (c *Client) setHeaders(req *http.Request, content bool) {
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	if content {
		req.Header.Set("content-type", "application/json")
	}
	// Cloudflare AI Gateway: authenticate to the gateway and (optionally) set a
	// response cache TTL. Both are no-ops when going direct to Anthropic.
	if c.gatewayAuth != "" {
		req.Header.Set("cf-aig-authorization", c.gatewayAuth)
	}
	if c.cacheTTL > 0 {
		req.Header.Set("cf-aig-cache-ttl", strconv.Itoa(c.cacheTTL))
	}
}

// resultFromMessage turns a decoded Messages response into a parsed Result.
func resultFromMessage(mr *messagesResponse, model string) (*Result, error) {
	if mr.Error != nil {
		return nil, fmt.Errorf("messages api error %s: %s", mr.Error.Type, mr.Error.Message)
	}
	var sb strings.Builder
	for _, block := range mr.Content {
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
			InputTokens:  mr.Usage.InputTokens,
			OutputTokens: mr.Usage.OutputTokens,
		},
		Model: model,
	}, nil
}

// parseDecisions extracts a JSON array of Decision from the model text, tolerant
// of accidental markdown fences or leading prose.
func parseDecisions(text string) ([]Decision, error) {
	text = stripFences(text)
	start := strings.IndexByte(text, '[')
	end := strings.LastIndexByte(text, ']')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("no JSON array found")
	}
	var ds []Decision
	if err := json.Unmarshal([]byte(text[start:end+1]), &ds); err != nil {
		return nil, err
	}
	return ds, nil
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
