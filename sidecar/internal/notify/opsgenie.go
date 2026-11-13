package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpsgenieSender delivers notifications through the Opsgenie Alert API
// v2 (market-14). The alias is the event's dedup key, so a resolve closes
// the alert its trigger opened. The channel's api_key is a secret.
type OpsgenieSender struct {
	client *http.Client
	// apiBase overrides the region's base URL (tests).
	apiBase string
}

// NewOpsgenieSenderWithPolicy creates an OpsgenieSender under policy.
func NewOpsgenieSenderWithPolicy(policy TargetPolicy) *OpsgenieSender {
	return &OpsgenieSender{client: policy.HTTPClient(10 * time.Second)}
}

// Type returns the channel type identifier.
func (s *OpsgenieSender) Type() string { return "opsgenie" }

// Opsgenie field limits: a longer message is cut by Opsgenie, a longer
// alias is rejected, so the alert would never open.
const (
	opsgenieMaxMessage     = 130
	opsgenieMaxAlias       = 512
	opsgenieMaxDescription = 15000
)

// OpsgenieBaseURL is the Alert API base URL for region: us (the default)
// or eu.
func OpsgenieBaseURL(region string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "", "us":
		return "https://api.opsgenie.com", nil
	case "eu":
		return "https://api.eu.opsgenie.com", nil
	}
	return "", fmt.Errorf("opsgenie region must be us or eu, got %q", region)
}

type opsgenieAlert struct {
	Message     string            `json:"message"`
	Alias       string            `json:"alias"`
	Description string            `json:"description,omitempty"`
	Priority    string            `json:"priority"`
	Source      string            `json:"source"`
	Entity      string            `json:"entity,omitempty"`
	Tags        []string          `json:"tags"`
	Details     map[string]string `json:"details"`
}

type opsgenieClose struct {
	Source string `json:"source"`
	Note   string `json:"note"`
}

// Send opens an alert, or closes it by alias when evt is a resolve.
func (s *OpsgenieSender) Send(ctx context.Context, ch Channel, evt Event) error {
	key := ch.Config["api_key"]
	if key == "" {
		return fmt.Errorf("opsgenie channel %q: missing api_key: %w", ch.Name, ErrPermanent)
	}
	base, err := OpsgenieBaseURL(ch.Config["region"])
	if err != nil {
		return fmt.Errorf("opsgenie channel %q: %w: %w", ch.Name, err, ErrPermanent)
	}
	if s.apiBase != "" {
		base = s.apiBase
	}
	alias := boundDedupKey(EventDedupKey(evt), opsgenieMaxAlias)
	target, body := opsgenieRequest(base, alias, evt)
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("opsgenie channel %q: marshal payload: %w", ch.Name, err)
	}
	headers := map[string]string{"Authorization": "GenieKey " + key}
	return RedactError(postJSON(ctx, s.client, "opsgenie", target, payload, headers))
}

func opsgenieRequest(base, alias string, evt Event) (string, any) {
	if evt.Resolve {
		return base + "/v2/alerts/" + url.PathEscape(alias) + "/close?identifierType=alias",
			opsgenieClose{Source: "pg_sage", Note: "Resolved by pg_sage: " + evt.Subject}
	}
	details := map[string]string{"event": evt.Type, "severity": evt.Severity}
	for k, v := range evt.Data {
		details[k] = fmt.Sprintf("%v", v)
	}
	entity, _ := evt.Data["database"].(string)
	return base + "/v2/alerts", opsgenieAlert{
		Message:     cutToRunes(evt.Subject, opsgenieMaxMessage),
		Alias:       alias,
		Description: cutToRunes(evt.Body, opsgenieMaxDescription),
		Priority:    opsgeniePriority(evt.Severity),
		Source:      "pg_sage",
		Entity:      entity,
		Tags:        []string{"pg_sage", evt.Type, evt.Severity},
		Details:     details,
	}
}

func opsgeniePriority(severity string) string {
	switch severity {
	case "critical":
		return "P1"
	case "warning":
		return "P3"
	default:
		return "P5"
	}
}

// cutToRunes keeps at most n runes of s.
func cutToRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
