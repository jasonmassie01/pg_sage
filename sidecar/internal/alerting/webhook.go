package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, w.url,
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook http post: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d",
			resp.StatusCode)
	}
	return nil
}
