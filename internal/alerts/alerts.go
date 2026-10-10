// Package alerts delivers operational alerts: to connected consoles (live
// event) and, when configured, to a webhook such as a Slack or Teams
// incoming webhook.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/relayops/apim/internal/realtime"
)

// Alert is one notification.
type Alert struct {
	Type     string         `json:"type"`     // e.g. mcp_definition_changed
	Severity string         `json:"severity"` // info, warning, critical
	Title    string         `json:"title"`
	Text     string         `json:"text"`
	TenantID string         `json:"tenant_id,omitempty"`
	Link     string         `json:"link,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	At       time.Time      `json:"at"`
}

// Notifier sends alerts. The zero value drops them.
type Notifier struct {
	Hub        *realtime.Hub
	WebhookURL string
	Client     *http.Client
}

// Notify publishes the alert to consoles and posts it to the webhook in the
// background; it never blocks the caller on the network.
func (n *Notifier) Notify(a Alert) {
	if n == nil {
		return
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	if n.Hub != nil {
		if a.TenantID != "" {
			n.Hub.PublishTenant("alert", a, a.TenantID)
		} else {
			n.Hub.Publish("alert", a)
		}
	}
	if n.WebhookURL == "" {
		return
	}
	go n.post(a)
}

func (n *Notifier) post(a Alert) {
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	text := "*" + a.Title + "*\n" + a.Text
	if a.Link != "" {
		text += "\n" + a.Link
	}
	// "text" is what Slack, Mattermost and Teams incoming webhooks display;
	// "alert" carries the structured fields for other receivers.
	body, _ := json.Marshal(map[string]any{"text": text, "alert": a})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.WebhookURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("alert webhook", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("alert webhook delivery failed", "type", a.Type, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("alert webhook rejected the alert", "type", a.Type, "status", resp.StatusCode)
	}
}
