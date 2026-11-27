package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/pg-sage/sidecar/internal/egress"
)

// WebhookSender posts every event as JSON to a generic HTTP endpoint
// (alerting-06). The channel's webhook_url and headers are secrets.
type WebhookSender struct {
	client *http.Client
	policy TargetPolicy
}

// NewWebhookSender creates a WebhookSender that refuses internal targets.
func NewWebhookSender() *WebhookSender {
	return NewWebhookSenderWithPolicy(TargetPolicy{})
}

// NewWebhookSenderWithPolicy creates a WebhookSender under policy.
func NewWebhookSenderWithPolicy(policy TargetPolicy) *WebhookSender {
	return &WebhookSender{client: policy.HTTPClient(10 * time.Second), policy: policy}
}

// Type returns the channel type identifier.
func (s *WebhookSender) Type() string { return "webhook" }

// webhookPayload is the body every webhook receives.
type webhookPayload struct {
	Event    string         `json:"event"`
	Severity string         `json:"severity"`
	Subject  string         `json:"subject"`
	Body     string         `json:"body,omitempty"`
	Database string         `json:"database,omitempty"`
	DedupKey string         `json:"dedup_key"`
	Resolve  bool           `json:"resolve"`
	Data     map[string]any `json:"data,omitempty"`
	SentAt   string         `json:"sent_at"`
}

// Send posts evt to the channel's webhook_url with its headers.
func (s *WebhookSender) Send(ctx context.Context, ch Channel, evt Event) error {
	url := ch.Config["webhook_url"]
	if url == "" {
		return fmt.Errorf("webhook channel %q: missing webhook_url: %w", ch.Name, ErrPermanent)
	}
	headers, err := ParseWebhookHeaders(ch.Config["headers"])
	if err != nil {
		return fmt.Errorf("webhook channel %q: %w: %w", ch.Name, err, ErrPermanent)
	}
	if err := s.policy.ValidateURL(url); err != nil {
		return fmt.Errorf("webhook channel %q: %w", ch.Name, err)
	}
	db, _ := evt.Data["database"].(string)
	body, err := json.Marshal(webhookPayload{Event: evt.Type, Severity: evt.Severity,
		Subject: evt.Subject, Body: evt.Body, Database: db, DedupKey: EventDedupKey(evt),
		Resolve: evt.Resolve, Data: evt.Data,
		SentAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return fmt.Errorf("webhook channel %q: marshal payload: %w", ch.Name, err)
	}
	return RedactError(postWebhook(ctx, s.client, url, body, headers))
}

// ParseWebhookHeaders reads a webhook channel's headers: empty, or a JSON
// object of valid header names to string values. Host is refused: it
// would redirect the request past the target check.
func ParseWebhookHeaders(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return nil, fmt.Errorf("headers must be a JSON object of strings: %w", err)
	}
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, fmt.Errorf("header name %q is not valid", name)
		}
		if strings.EqualFold(name, "Host") {
			return nil, fmt.Errorf("header %q cannot be set", name)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("header %q has an invalid value", name)
		}
	}
	return headers, nil
}

func postWebhook(
	ctx context.Context, client *http.Client, url string, body []byte,
	headers map[string]string,
) error {
	return postJSON(ctx, client, "webhook", url, body, headers)
}

// HTTPStatusError is a non-2xx answer from a notification endpoint.
type HTTPStatusError struct {
	Service string
	Code    int
	// RetryAfter is the endpoint's Retry-After hint; 0 when it sent none.
	RetryAfter time.Duration
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s returned status %d", e.Service, e.Code)
}

// Retryable reports whether another attempt can succeed: network errors,
// 5xx and 429 are; other 4xx answers, refused targets and invalid
// channel configuration are not (ported from the YAML stack, G7-B32).
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, egress.ErrBlocked) || errors.Is(err, ErrTargetBlocked) ||
		errors.Is(err, ErrPermanent) {
		return false
	}
	var se *HTTPStatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return true
}

// ErrPermanent marks a delivery failure that no retry can fix.
var ErrPermanent = errors.New("permanent delivery failure")
