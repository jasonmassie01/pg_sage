package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Slack Block Kit limits (G7-B15).
const (
	slackHeaderMax  = 150
	slackSectionMax = 3000
)

// SlackChannel sends alerts via Slack webhook with Block Kit.
type SlackChannel struct {
	webhookURL string
	client     *http.Client
	logFn      func(string, string, ...any)
}

// NewSlack creates a SlackChannel. client should come from the
// notification target policy; nil applies the strict policy (alerting-11).
func NewSlack(
	webhookURL string,
	client *http.Client,
	logFn func(string, string, ...any),
) *SlackChannel {
	return &SlackChannel{
		webhookURL: webhookURL,
		client:     policyClient(client),
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
	return sendWithRetry(ctx, "slack", s.logFn, func() error {
		return s.doPost(ctx, payload)
	})
}

func (s *SlackChannel) doPost(
	ctx context.Context, payload []byte,
) error {
	return postJSON(ctx, s.client, "slack", s.webhookURL, payload, nil)
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

// buildPayload constructs a Slack Block Kit message. Untrusted text
// (titles, identifiers, recommendations) is mrkdwn-escaped and every
// block is kept within Slack's limits (G7-B15, G7-B31).
func (s *SlackChannel) buildPayload(
	alert Alert,
) ([]byte, error) {
	header := fmt.Sprintf("%s pg_sage%s: %d %s finding(s)",
		severityEmoji(alert.Severity), databaseLabel(alert.Database),
		len(alert.Findings), alert.Severity)
	switch {
	case alert.Resolved:
		header = fmt.Sprintf("pg_sage%s: resolved %d finding(s)",
			databaseLabel(alert.Database), len(alert.Findings))
	case alert.Reminder && len(alert.Findings) > 0:
		header = fmt.Sprintf("%s pg_sage%s: Still open for %s (seen %d times)",
			severityEmoji(alert.Severity), databaseLabel(alert.Database),
			formatOpenFor(alert.OpenFor), alert.Findings[0].OccurrenceCount)
	}
	blocks := []map[string]any{{
		"type": "header",
		"text": map[string]any{
			"type": "plain_text",
			"text": notify.TruncateRunes(header, slackHeaderMax),
		},
	}}
	for _, f := range alert.Findings {
		text := fmt.Sprintf(
			"*%s*\nObject: `%s` (%s)\nSeen %d time(s)\n%s",
			notify.EscapeMrkdwn(f.Title),
			notify.EscapeMrkdwn(f.ObjectIdentifier),
			notify.EscapeMrkdwn(f.ObjectType), f.OccurrenceCount,
			notify.EscapeMrkdwn(f.Recommendation),
		)
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": notify.TruncateRunes(text, slackSectionMax),
			},
		})
	}
	return json.Marshal(map[string]any{"blocks": blocks})
}

// formatOpenFor renders a reminder's open time in whole minutes: "45m",
// "3h20m", or "2d3h" from a day on.
func formatOpenFor(d time.Duration) string {
	mins := int64(d / time.Minute)
	if mins < 0 {
		mins = 0
	}
	switch {
	case mins >= 24*60:
		return fmt.Sprintf("%dd%dh", mins/(24*60), mins%(24*60)/60)
	case mins >= 60:
		return fmt.Sprintf("%dh%dm", mins/60, mins%60)
	}
	return fmt.Sprintf("%dm", mins)
}

func databaseLabel(db string) string {
	if db == "" {
		return ""
	}
	return " [" + db + "]"
}
