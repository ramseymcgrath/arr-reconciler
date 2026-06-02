// Package notify delivers run summaries to a webhook (Discord/Slack-compatible).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// discordContentLimit is the maximum length Discord accepts for a webhook
// message's content field. Slack is more generous, so honoring Discord's cap
// keeps a single payload valid for both.
const discordContentLimit = 2000

// Notifier posts text messages to a webhook URL. A Notifier with an empty URL
// is a no-op, so callers can always construct one and call Send unconditionally.
type Notifier struct {
	url  string
	http *http.Client
}

// New returns a Notifier for the given webhook URL. If url is empty, the
// returned Notifier silently does nothing on Send.
func New(url string, timeout time.Duration) *Notifier {
	return &Notifier{
		url:  url,
		http: &http.Client{Timeout: timeout},
	}
}

// Send posts text to the configured webhook. It populates both "content"
// (Discord) and "text" (Slack) so the same payload works against either
// service. When no URL is configured it returns nil without making a request.
func (n *Notifier) Send(ctx context.Context, text string) error {
	if n.url == "" || text == "" {
		return nil
	}

	content := truncate(text, discordContentLimit)

	payload := map[string]string{
		"content": content, // Discord
		"text":    content, // Slack
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("webhook status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// truncate shortens s to at most max bytes, appending an ellipsis and cutting
// on a UTF-8 rune boundary so a multibyte character is never split.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "..."
	cut := max - len(ellipsis)
	if cut < 0 {
		cut = 0
	}
	// Back up to a rune boundary (bytes 0x80–0xBF are UTF-8 continuation bytes).
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + ellipsis
}
