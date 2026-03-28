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

// SlackSender delivers notifications via Slack incoming webhooks.
type SlackSender struct {
	client *http.Client
}

// NewSlackSender creates a SlackSender with a default HTTP client.
func NewSlackSender() *SlackSender {
	return &SlackSender{
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Type returns the channel type identifier.
func (s *SlackSender) Type() string { return "slack" }

// Send posts a rich message to the Slack webhook URL.
func (s *SlackSender) Send(
	ctx context.Context, ch Channel, evt Event,
) error {
	webhookURL := ch.Config["webhook_url"]
	if webhookURL == "" {
		return fmt.Errorf("slack channel %q: missing webhook_url",
			ch.Name)
	}

	payload, err := buildSlackPayload(evt)
	if err != nil {
		return fmt.Errorf("build slack payload: %w", err)
	}

	return postSlackWebhook(ctx, s.client, webhookURL, payload)
}

func buildSlackPayload(evt Event) ([]byte, error) {
	emoji := notifySeverityEmoji(evt.Severity)
	header := fmt.Sprintf("%s %s", emoji, evt.Subject)

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{
				"type": "plain_text",
				"text": header,
			},
		},
	}

	if evt.Body != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": evt.Body,
			},
		})
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{
				"type": "mrkdwn",
				"text": fmt.Sprintf(
					"*Event:* %s | *Severity:* %s | %s",
					evt.Type, evt.Severity, ts),
			},
		},
	})

	return json.Marshal(map[string]any{"blocks": blocks})
}

func postSlackWebhook(
	ctx context.Context, client *http.Client,
	url string, payload []byte,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url,
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack http post: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack returned status %d",
			resp.StatusCode)
	}
	return nil
}

func notifySeverityEmoji(sev string) string {
	switch sev {
	case "critical":
		return "\xf0\x9f\x94\xb4" // red circle
	case "warning":
		return "\xe2\x9a\xa0\xef\xb8\x8f" // warning sign
	default:
		return "\xe2\x84\xb9\xef\xb8\x8f" // info
	}
}
