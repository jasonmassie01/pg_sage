package specialist

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// PagerDuty v3 incident webhooks (no vendor SDK): incident.triggered and
// incident.reopened open an investigation of the database the incident's
// service maps to; the result goes back as an incident note when an
// outbound PagerDuty integration is configured. A request needs a bearer
// MCP token (a custom header on the subscription) and a valid
// X-PagerDuty-Signature.

// maxAdapterBody bounds an adapter request (PagerDuty payloads carry the
// whole incident).
const maxAdapterBody = 256 << 10

// PagerDutyRoute maps one PagerDuty service to a database and an optional
// incident family.
type PagerDutyRoute struct {
	Database string
	Family   string
}

// PagerDutyAdapter verifies and translates PagerDuty webhooks.
type PagerDutyAdapter struct {
	Secret   string
	Services map[string]PagerDutyRoute
}

var pdServiceID = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// ParsePagerDutyServices parses SERVICE_ID=database[:family] entries.
func ParsePagerDutyServices(entries []string) (map[string]PagerDutyRoute, error) {
	out := make(map[string]PagerDutyRoute, len(entries))
	for _, entry := range entries {
		service, target, ok := strings.Cut(entry, "=")
		service, target = strings.TrimSpace(service), strings.TrimSpace(target)
		database, family, _ := strings.Cut(target, ":")
		switch {
		case !ok || !pdServiceID.MatchString(service):
			return nil, invalidf("pagerduty service mapping %q: SERVICE_ID=database", entry)
		case !databaseName.MatchString(database):
			return nil, invalidf("pagerduty service mapping %q: invalid database", entry)
		case family != "" && families[family] == "":
			return nil, invalidf("pagerduty service mapping %q: unknown family %q", entry,
				family)
		}
		if _, dup := out[service]; dup {
			return nil, invalidf("pagerduty service %s is mapped twice", service)
		}
		out[service] = PagerDutyRoute{Database: database, Family: family}
	}
	return out, nil
}

// VerifyPagerDutySignature checks X-PagerDuty-Signature: one or more
// comma-separated v1=<hex HMAC-SHA256 of the body>; any match passes.
func VerifyPagerDutySignature(secret string, body []byte, header string) error {
	if secret == "" || header == "" {
		return ErrSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, part := range strings.Split(header, ",") {
		hexSig, ok := strings.CutPrefix(strings.TrimSpace(part), "v1=")
		if !ok {
			continue
		}
		got, err := hex.DecodeString(hexSig)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrSignature
}

type pagerDutyEvent struct {
	Event struct {
		EventType  string    `json:"event_type"`
		OccurredAt time.Time `json:"occurred_at"`
		Data       struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
			Service struct {
				ID string `json:"id"`
			} `json:"service"`
		} `json:"data"`
	} `json:"event"`
}

// translate maps an event to (database, request), or an ignore reason.
func (a *PagerDutyAdapter) translate(body []byte, now time.Time) (string, OpenRequest,
	string, error) {
	var ev pagerDutyEvent
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&ev); err != nil {
		return "", OpenRequest{}, "", invalidf("pagerduty event: %v", err)
	}
	e := ev.Event
	if e.EventType != "incident.triggered" && e.EventType != "incident.reopened" {
		return "", OpenRequest{}, "event type " + safeLabel(e.EventType) +
			" does not open an investigation", nil
	}
	route, ok := a.Services[e.Data.Service.ID]
	if !ok {
		return "", OpenRequest{}, "service " + safeLabel(e.Data.Service.ID) +
			" is not mapped to a database", nil
	}
	if !pdServiceID.MatchString(e.Data.ID) {
		return "", OpenRequest{}, "", invalidf("pagerduty incident id is malformed")
	}
	title := dataText(e.Data.Title, maxSummaryRunes)
	if title == "" {
		title = "PagerDuty incident " + e.Data.ID
	}
	req := OpenRequest{Symptom: &Symptom{Summary: title}, Family: route.Family,
		ExternalRef:    &ExternalRef{System: "pagerduty", ID: e.Data.ID},
		IdempotencyKey: "pagerduty:" + e.Data.ID}
	if strings.HasPrefix(e.Data.HTMLURL, "https://") && len(e.Data.HTMLURL) <= maxExternalURL {
		req.ExternalRef.URL = e.Data.HTMLURL
	}
	if !e.OccurredAt.IsZero() {
		start := e.OccurredAt
		req.Window = &Window{Start: start}
		if req.validateWindow(now) != nil {
			req.Window = nil // a reopened old incident: no window
		}
	}
	return route.Database, req, "", nil
}

// dataText keeps caller text as data: control characters become spaces
// and it is bounded.
func dataText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// safeLabel bounds an echoed event value.
func safeLabel(s string) string {
	return "\"" + dataText(s, 64) + "\""
}

func (h *handler) pagerDuty(w http.ResponseWriter, r *http.Request, id Identity) {
	a := h.opts.PagerDuty
	if a == nil {
		writeError(w, ErrDisabled)
		return
	}
	body, err := readAdapterBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := VerifyPagerDutySignature(a.Secret, body,
		r.Header.Get("X-PagerDuty-Signature")); err != nil {
		writeError(w, err)
		return
	}
	database, req, ignored, err := a.translate(body, h.opts.Now())
	switch {
	case err != nil:
		writeError(w, err)
	case ignored != "":
		writeJSON(w, http.StatusAccepted, WebhookIgnored{ContractVersion: ContractVersion,
			Ignored: true, Reason: ignored})
	default:
		h.respondOpen(w, r.Context(), id, database, req)
	}
}

func readAdapterBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdapterBody+1))
	if err != nil {
		return nil, invalidf("reading the body: %v", err)
	}
	if len(body) > maxAdapterBody {
		return nil, ErrTooLarge
	}
	return body, nil
}
