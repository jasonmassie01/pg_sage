package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

const pdEventsURL = "https://events.pagerduty.com/v2/enqueue"

// PagerDutyChannel sends alerts via PagerDuty Events API v2.
type PagerDutyChannel struct {
	routingKey string
	client     *http.Client
	logFn      func(string, string, ...any)
}

// NewPagerDuty creates a PagerDutyChannel. The fixed Events API endpoint
// goes through client too, so proxy and redirect handling match the other
// channels; nil applies the strict target policy (alerting-11).
func NewPagerDuty(
	routingKey string,
	client *http.Client,
	logFn func(string, string, ...any),
) *PagerDutyChannel {
	return &PagerDutyChannel{
		routingKey: routingKey,
		client:     policyClient(client),
		logFn:      logFn,
	}
}

// Name returns the channel identifier.
func (p *PagerDutyChannel) Name() string { return "pagerduty" }

// Send dispatches an alert to PagerDuty with retry.
func (p *PagerDutyChannel) Send(
	ctx context.Context, alert Alert,
) error {
	payload, err := p.buildPayload(alert)
	if err != nil {
		return fmt.Errorf("build pagerduty payload: %w", err)
	}
	return sendWithRetry(ctx, "pagerduty", p.logFn, func() error {
		return p.doPost(ctx, payload)
	})
}

func (p *PagerDutyChannel) doPost(
	ctx context.Context, payload []byte,
) error {
	return postJSON(ctx, p.client, "pagerduty", pdEventsURL, payload, nil)
}

// pagerDutyDedupKey is <database>:<category>:<object> for a named database
// and <category>:<object> otherwise, bounded to the Events API limit.
// Identical schemas in a fleet that share a routing key would otherwise
// merge into one incident, and one database's resolve would close another
// database's page (alerting-02). Trigger and resolve use the same key.
func pagerDutyDedupKey(alert Alert) string {
	first := alert.Findings[0]
	key := FormatDedupKey(first.Category, first.ObjectIdentifier)
	if alert.Database != "" {
		key = alert.Database + ":" + key
	}
	return notify.BoundPagerDutyDedupKey(key)
}

func (p *PagerDutyChannel) buildPayload(
	alert Alert,
) ([]byte, error) {
	if len(alert.Findings) == 0 {
		return nil, fmt.Errorf("no findings in alert")
	}

	dedupKey := pagerDutyDedupKey(alert)

	titles := make([]string, 0, len(alert.Findings))
	for _, f := range alert.Findings {
		titles = append(titles, f.Title)
	}
	summary := fmt.Sprintf("pg_sage: %s",
		strings.Join(titles, "; "))
	if len(summary) > 1024 {
		summary = summary[:1021] + "..."
	}

	action := "trigger"
	if alert.Resolved {
		action = "resolve"
	}
	source := "pg_sage"
	if alert.Database != "" {
		source = "pg_sage:" + alert.Database
	}
	ev := map[string]any{
		"routing_key":  p.routingKey,
		"event_action": action,
		"dedup_key":    dedupKey,
		"payload": map[string]any{
			"summary":   summary,
			"source":    source,
			"component": alert.Database,
			"severity":  alert.Severity,
			"timestamp": alert.Timestamp.Format(time.RFC3339),
		},
	}
	return json.Marshal(ev)
}
