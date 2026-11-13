package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"
)

const pdEventsURL = "https://events.pagerduty.com/v2/enqueue"

// PagerDutySender delivers notifications via PagerDuty Events API v2.
type PagerDutySender struct {
	client *http.Client
	apiURL string
}

// NewPagerDutySender creates a PagerDutySender with default settings.
func NewPagerDutySender() *PagerDutySender {
	return &PagerDutySender{
		client: TargetPolicy{}.HTTPClient(10 * time.Second),
		apiURL: pdEventsURL,
	}
}

// Type returns the channel type identifier.
func (p *PagerDutySender) Type() string { return "pagerduty" }

// Send posts an event to PagerDuty via Events API v2.
func (p *PagerDutySender) Send(
	ctx context.Context, ch Channel, evt Event,
) error {
	routingKey := ch.Config["routing_key"]
	if routingKey == "" {
		return fmt.Errorf(
			"pagerduty channel %q: missing routing_key", ch.Name)
	}

	payload, err := buildPagerDutyPayload(ch, evt, routingKey)
	if err != nil {
		return fmt.Errorf("build pagerduty payload: %w", err)
	}

	return RedactError(postPagerDuty(ctx, p.client, p.apiURL, payload))
}

type pdEvent struct {
	RoutingKey  string    `json:"routing_key"`
	EventAction string    `json:"event_action"`
	DedupKey    string    `json:"dedup_key,omitempty"`
	Payload     pdPayload `json:"payload"`
}

type pdPayload struct {
	Summary       string         `json:"summary"`
	Source        string         `json:"source"`
	Severity      string         `json:"severity"`
	Timestamp     string         `json:"timestamp,omitempty"`
	Component     string         `json:"component,omitempty"`
	Group         string         `json:"group,omitempty"`
	Class         string         `json:"class,omitempty"`
	CustomDetails map[string]any `json:"custom_details,omitempty"`
}

func buildPagerDutyPayload(
	ch Channel, evt Event, routingKey string,
) ([]byte, error) {
	source := ch.Config["source"]
	if source == "" {
		source = "pg_sage"
	}

	component := ch.Config["component"]
	if component == "" {
		if db, ok := evt.Data["database"]; ok {
			component = fmt.Sprintf("%v", db)
		}
	}

	dedupKey := EventDedupKey(evt)
	if prefix := ch.Config["dedup_key_prefix"]; prefix != "" {
		dedupKey = prefix + ":" + dedupKey
	}
	dedupKey = BoundPagerDutyDedupKey(dedupKey)
	action := "trigger"
	if evt.Resolve {
		action = "resolve"
	}

	pd := pdEvent{
		RoutingKey:  routingKey,
		EventAction: action,
		DedupKey:    dedupKey,
		Payload: pdPayload{
			Summary:       evt.Subject,
			Source:        source,
			Severity:      mapPDSeverity(evt.Severity),
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			Component:     component,
			Class:         evt.Type,
			CustomDetails: evt.Data,
		},
	}

	return json.Marshal(pd)
}

// pdMaxDedupKey is the Events API v2 limit on dedup_key length; PagerDuty
// rejects a longer key, so the page would never be delivered.
const pdMaxDedupKey = 255

// BoundPagerDutyDedupKey keeps key within pdMaxDedupKey (boundDedupKey).
func BoundPagerDutyDedupKey(key string) string {
	return boundDedupKey(key, pdMaxDedupKey)
}

// boundDedupKey keeps key within limit bytes. A longer key keeps its
// readable head, cut on a rune boundary, plus a hash of the whole key, so
// the trigger and the resolve of one identity still share it.
func boundDedupKey(key string, limit int) string {
	if len(key) <= limit {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	tail := ":" + hex.EncodeToString(sum[:8])
	cut := limit - len(tail)
	for cut > 0 && !utf8.RuneStart(key[cut]) {
		cut--
	}
	return key[:cut] + tail
}

func mapPDSeverity(sev string) string {
	switch sev {
	case "critical":
		return "critical"
	case "warning":
		return "warning"
	default:
		return "info"
	}
}

func postPagerDuty(
	ctx context.Context, client *http.Client,
	url string, payload []byte,
) error {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, url,
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create pagerduty request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pagerduty http post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	// PagerDuty Events API v2 returns 202 Accepted on success.
	if resp.StatusCode != http.StatusAccepted {
		return &HTTPStatusError{Service: "pagerduty", Code: resp.StatusCode}
	}
	return nil
}
