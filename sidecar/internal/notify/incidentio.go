package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// incidentIOBase is the incident.io API; alert events go to an HTTP alert
// source named by its config id.
const incidentIOBase = "https://api.incident.io"

// IncidentIOSender posts HTTP alert source events to incident.io
// (market-14). The deduplication key is the event's dedup key, and a
// resolve sends status "resolved". The channel's token is a secret.
type IncidentIOSender struct {
	client *http.Client
	// apiBase overrides incidentIOBase (tests).
	apiBase string
}

// NewIncidentIOSenderWithPolicy creates an IncidentIOSender under policy.
func NewIncidentIOSenderWithPolicy(policy TargetPolicy) *IncidentIOSender {
	return &IncidentIOSender{client: policy.HTTPClient(10 * time.Second),
		apiBase: incidentIOBase}
}

// Type returns the channel type identifier.
func (s *IncidentIOSender) Type() string { return "incidentio" }

var incidentIOSourceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidIncidentIOSourceID reports whether id can name an alert source: it
// goes into the URL path, so only letters, digits, '_' and '-' are kept.
func ValidIncidentIOSourceID(id string) bool {
	return incidentIOSourceID.MatchString(id)
}

type incidentIOEvent struct {
	Title            string         `json:"title"`
	Description      string         `json:"description,omitempty"`
	DeduplicationKey string         `json:"deduplication_key"`
	Status           string         `json:"status"`
	Metadata         map[string]any `json:"metadata"`
}

// Send posts evt as a firing or resolved alert event.
func (s *IncidentIOSender) Send(ctx context.Context, ch Channel, evt Event) error {
	token := ch.Config["token"]
	if token == "" {
		return fmt.Errorf("incidentio channel %q: missing token: %w", ch.Name, ErrPermanent)
	}
	source := ch.Config["alert_source_config_id"]
	if !ValidIncidentIOSourceID(source) {
		return fmt.Errorf("incidentio channel %q: alert_source_config_id must be 1-64 "+
			"letters, digits, '_' or '-': %w", ch.Name, ErrPermanent)
	}
	payload, err := json.Marshal(incidentIOEventFor(evt))
	if err != nil {
		return fmt.Errorf("incidentio channel %q: marshal payload: %w", ch.Name, err)
	}
	target := strings.TrimSuffix(s.apiBase, "/") + "/v2/alert_events/http/" + source
	headers := map[string]string{"Authorization": "Bearer " + token}
	return RedactError(postJSON(ctx, s.client, "incidentio", target, payload, headers))
}

func incidentIOEventFor(evt Event) incidentIOEvent {
	meta := map[string]any{"event": evt.Type, "severity": evt.Severity}
	for k, v := range evt.Data {
		meta[k] = v
	}
	status := "firing"
	if evt.Resolve {
		status = "resolved"
	}
	return incidentIOEvent{Title: evt.Subject, Description: evt.Body,
		DeduplicationKey: EventDedupKey(evt), Status: status, Metadata: meta}
}
