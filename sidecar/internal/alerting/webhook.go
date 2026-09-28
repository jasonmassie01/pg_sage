package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// WebhookChannel sends alerts to a generic HTTP endpoint.
type WebhookChannel struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client
	logFn   func(string, string, ...any)
}

// NewWebhook creates a generic webhook channel.
func NewWebhook(
	name, url string,
	headers map[string]string,
	logFn func(string, string, ...any),
) *WebhookChannel {
	return &WebhookChannel{
		name:    "webhook:" + name,
		url:     url,
		headers: headers,
		client:  &http.Client{Timeout: 10 * time.Second},
		logFn:   logFn,
	}
}

// Name returns the configured channel name.
func (w *WebhookChannel) Name() string { return w.name }

// Send dispatches an alert to the webhook endpoint.
func (w *WebhookChannel) Send(
	ctx context.Context, alert Alert,
) error {
	body, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	return postJSON(ctx, w.client, "webhook", w.url, body, w.headers)
}
