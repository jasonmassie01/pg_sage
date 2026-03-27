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

// SlackChannel sends alerts via Slack webhook with Block Kit.
type SlackChannel struct {
	webhookURL string
	client     *http.Client
	logFn      func(string, string, ...any)
}

// NewSlack creates a SlackChannel.
func NewSlack(
	webhookURL string,
	logFn func(string, string, ...any),
) *SlackChannel {
	return &SlackChannel{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
		logFn:      logFn,
	}
}

// Name returns the channel identifier.
func (s *SlackChannel) Name() string { return "slack" }

// Send dispatches an alert to Slack.
func (s *SlackChannel) Send(
	ctx context.Context, alert Alert,
) error {
	payload, err := s.buildPayload(alert)
	if err != nil {
		return fmt.Errorf("build slack payload: %w", err)
	}
	return s.sendWithRetry(ctx, payload)
}

func (s *SlackChannel) sendWithRetry(
	ctx context.Context, payload []byte,
) error {
	const maxAttempts = 3
	backoff := 1 * time.Second

	var lastErr error
	for i := range maxAttempts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("slack send cancelled: %w", err)
		}

		lastErr = s.doPost(ctx, payload)
		if lastErr == nil {
			return nil
		}

		if i < maxAttempts-1 {
			s.logFn("WARN", "slack retry %d/%d: %v",
				i+1, maxAttempts, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("slack send cancelled: %w",
					ctx.Err())
			}
			backoff *= 2
		}
	}
	return fmt.Errorf("slack send failed after %d attempts: %w",
		maxAttempts, lastErr)
}

func (s *SlackChannel) doPost(
	ctx context.Context, payload []byte,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, s.webhookURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("create slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack http post: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack returned status %d",
			resp.StatusCode)
	}
	return nil
}

func severityEmoji(sev string) string {
	switch sev {
	case "critical":
		return "\xf0\x9f\x94\xb4" // red circle
	case "warning":
		return "\xe2\x9a\xa0\xef\xb8\x8f" // warning sign
	default:
		return "\xe2\x84\xb9\xef\xb8\x8f" // info
	}
}

// buildPayload constructs a Slack Block Kit message.
func (s *SlackChannel) buildPayload(
	alert Alert,
) ([]byte, error) {
	emoji := severityEmoji(alert.Severity)
	header := fmt.Sprintf("%s pg_sage: %d %s finding(s)",
		emoji, len(alert.Findings), alert.Severity)

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{
				"type": "plain_text",
				"text": header,
			},
		},
	}

	for _, f := range alert.Findings {
		text := fmt.Sprintf(
			"*%s*\nObject: `%s` (%s)\nSeen %d time(s)\n%s",
			f.Title, f.ObjectIdentifier,
			f.ObjectType, f.OccurrenceCount,
			f.Recommendation,
		)
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": text,
			},
		})
	}

	payload := map[string]any{"blocks": blocks}
	return json.Marshal(payload)
}
