package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const pdEventsURL = "https://events.pagerduty.com/v2/enqueue"

// PagerDutyChannel sends alerts via PagerDuty Events API v2.
type PagerDutyChannel struct {
	routingKey string
	client     *http.Client
	logFn      func(string, string, ...any)
}

// NewPagerDuty creates a PagerDutyChannel.
func NewPagerDuty(
	routingKey string,
	logFn func(string, string, ...any),
) *PagerDutyChannel {
	return &PagerDutyChannel{
		routingKey: routingKey,
		client:     &http.Client{Timeout: 10 * time.Second},
		logFn:      logFn,
	}
}

// Name returns the channel identifier.
func (p *PagerDutyChannel) Name() string { return "pagerduty" }

// Send dispatches an alert to PagerDuty.
func (p *PagerDutyChannel) Send(
	ctx context.Context, alert Alert,
) error {
	payload, err := p.buildPayload(alert)
	if err != nil {
		return fmt.Errorf("build pagerduty payload: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, pdEventsURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return fmt.Errorf("create pagerduty request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty http post: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pagerduty returned status %d",
			resp.StatusCode)
	}
	return nil
}

func (p *PagerDutyChannel) buildPayload(
	alert Alert,
) ([]byte, error) {
	if len(alert.Findings) == 0 {
		return nil, fmt.Errorf("no findings in alert")
	}

	first := alert.Findings[0]
	dedupKey := FormatDedupKey(
		first.Category, first.ObjectIdentifier,
	)

	titles := make([]string, 0, len(alert.Findings))
	for _, f := range alert.Findings {
		titles = append(titles, f.Title)
	}
	summary := fmt.Sprintf("pg_sage: %s",
		strings.Join(titles, "; "))
	if len(summary) > 1024 {
		summary = summary[:1021] + "..."
	}

	ev := map[string]any{
		"routing_key":  p.routingKey,
		"event_action": "trigger",
		"dedup_key":    dedupKey,
		"payload": map[string]any{
			"summary":   summary,
			"source":    "pg_sage",
			"severity":  alert.Severity,
			"timestamp": alert.Timestamp.Format(time.RFC3339),
		},
	}
	return json.Marshal(ev)
}
