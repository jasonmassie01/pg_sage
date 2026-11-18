package specialist

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// incident.io webhooks (mcp-02), no vendor SDK. incident.io signs its
// webhooks the Svix way: webhook-id, webhook-timestamp (unix seconds) and
// webhook-signature, a space-separated list of "v1,<base64 HMAC-SHA256 of
// id.timestamp.body>" keyed by the endpoint secret "whsec_<base64>". An
// incident_created event opens an investigation of the database its
// custom field (default "Database") names through the configured routes.
// The bearer MCP token is still required, as on every adapter.

// incidentIOTolerance is the accepted age and skew of a signed delivery.
const incidentIOTolerance = 5 * time.Minute

// incidentIOOpens are the event types that open an investigation.
var incidentIOOpens = map[string]bool{"public_incident.incident_created_v2": true}

var incidentIOID = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// DefaultIncidentIOField is the custom field that names the database.
const DefaultIncidentIOField = "Database"

// IncidentIOAdapter verifies and translates incident.io webhooks.
type IncidentIOAdapter struct {
	Secret string
	// Field is the custom field whose value selects the route ("" is
	// DefaultIncidentIOField).
	Field  string
	Routes map[string]PagerDutyRoute // by lower-cased field value
}

func (a *IncidentIOAdapter) field() string {
	if a.Field == "" {
		return DefaultIncidentIOField
	}
	return a.Field
}

// VerifyIncidentIOSignature checks a delivery's signature and that its
// timestamp is within five minutes of now (inclusive); any one matching
// v1 signature passes (secret rotation).
func VerifyIncidentIOSignature(secret string, body []byte, id, timestamp, signature string,
	now time.Time) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 || id == "" || signature == "" {
		return ErrSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrSignature
	}
	if skew := now.Sub(time.Unix(ts, 0)); skew > incidentIOTolerance ||
		skew < -incidentIOTolerance {
		return ErrSignature
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, part := range strings.Fields(signature) {
		b64, ok := strings.CutPrefix(part, "v1,")
		if !ok {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(b64)
		if err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrSignature
}

type incidentIOIncident struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Reference   string    `json:"reference"`
	Permalink   string    `json:"permalink"`
	CreatedAt   time.Time `json:"created_at"`
	FieldValues []struct {
		Field struct {
			Name string `json:"name"`
		} `json:"custom_field"`
		Values []struct {
			Option *struct {
				Value string `json:"value"`
			} `json:"value_option"`
			Text string `json:"value_text"`
		} `json:"values"`
	} `json:"custom_field_entries"`
}

// incidentIOEvent decodes the envelope: the incident sits under a key
// named like the event type.
func incidentIOEvent(body []byte) (string, *incidentIOIncident, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return "", nil, invalidf("incident.io event: %v", err)
	}
	var eventType string
	if err := json.Unmarshal(env["event_type"], &eventType); err != nil || eventType == "" {
		return "", nil, invalidf("incident.io event: event_type is required")
	}
	if !incidentIOOpens[eventType] {
		return eventType, nil, nil
	}
	raw, ok := env[eventType]
	var inc incidentIOIncident
	if !ok || json.Unmarshal(raw, &inc) != nil {
		return "", nil, invalidf("incident.io event: the %s object is missing or "+
			"malformed", eventType)
	}
	if !incidentIOID.MatchString(inc.ID) {
		return "", nil, invalidf("incident.io event: %s.id is malformed", eventType)
	}
	return eventType, &inc, nil
}

// fieldValue is the incident's value of the routing field.
func (a *IncidentIOAdapter) fieldValue(inc *incidentIOIncident) string {
	for _, entry := range inc.FieldValues {
		if !strings.EqualFold(strings.TrimSpace(entry.Field.Name), a.field()) {
			continue
		}
		for _, v := range entry.Values {
			if v.Option != nil && strings.TrimSpace(v.Option.Value) != "" {
				return strings.TrimSpace(v.Option.Value)
			}
			if t := strings.TrimSpace(v.Text); t != "" {
				return t
			}
		}
	}
	return ""
}

// request is the open request of a routed incident.
func (a *IncidentIOAdapter) request(inc *incidentIOIncident, route PagerDutyRoute,
	now time.Time) OpenRequest {
	title := dataText(inc.Name, maxSummaryRunes)
	if title == "" {
		title = "incident.io incident " + inc.ID
	}
	req := OpenRequest{Symptom: &Symptom{Summary: title}, Family: route.Family,
		ExternalRef:    &ExternalRef{System: "incident_io", ID: inc.ID},
		IdempotencyKey: "incidentio:" + inc.ID}
	if strings.HasPrefix(inc.Permalink, "https://") && len(inc.Permalink) <= maxExternalURL {
		req.ExternalRef.URL = inc.Permalink
	}
	return withAlertWindow(req, inc.CreatedAt, now)
}

// withAlertWindow sets the window an alert started at, unless the
// contract would refuse it (an old incident reopened): then none.
func withAlertWindow(req OpenRequest, start, now time.Time) OpenRequest {
	if start.IsZero() {
		return req
	}
	req.Window = &Window{Start: start}
	if req.validateWindow(now) != nil {
		req.Window = nil
	}
	return req
}

func (h *handler) incidentIO(w http.ResponseWriter, r *http.Request, id Identity) {
	a := h.opts.IncidentIO
	if a == nil {
		writeError(w, ErrDisabled)
		return
	}
	body, err := readAdapterBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := VerifyIncidentIOSignature(a.Secret, body, r.Header.Get("webhook-id"),
		r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"),
		h.opts.Now()); err != nil {
		writeError(w, err)
		return
	}
	eventType, inc, err := incidentIOEvent(body)
	switch {
	case err != nil:
		writeError(w, err)
		return
	case inc == nil:
		ignoreEvent(w, "event type "+safeLabel(eventType)+" does not open an investigation")
		return
	}
	value := a.fieldValue(inc)
	route, ok := a.Routes[strings.ToLower(value)]
	if !ok {
		ignoreUnmapped(w, "incident "+safeLabel(inc.ID)+" (its "+safeLabel(a.field())+
			" field value "+safeLabel(value)+")", "specialist.incidentio.routes")
		return
	}
	h.respondOpen(w, r.Context(), id, route.Database, a.request(inc, route, h.opts.Now()))
}
