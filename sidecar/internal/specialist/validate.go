package specialist

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Caller input is strict (unknown fields refused), bounded and data only.

// MaxBodyBytes bounds a contract request body.
const MaxBodyBytes = 16 << 10

// Request field limits (the published schema's).
const (
	maxSummaryRunes     = 512
	maxDescriptionRunes = 4000
	maxIncidentRunes    = 256
	maxExternalIDRunes  = 256
	maxExternalURL      = 2048
	maxIdempotencyRunes = 128
	maxReasonRunes      = 1000
	windowFutureSkew    = 5 * time.Minute
	windowMaxAge        = 7 * 24 * time.Hour
)

// families are the incident families a caller may name; each is a trigger
// kind of the investigator.
var families = map[string]sre.TriggerKind{
	"lock_blocking": sre.TriggerLock, "connection_pressure": sre.TriggerConnections,
	"wal_retention": sre.TriggerWAL, "plan_regression": sre.TriggerPlan,
	"checkpoint_storm": sre.TriggerCheckpoint, "temp_file_explosion": sre.TriggerTempFiles,
	"replication_lag": sre.TriggerReplicationLag, "lwlock_contention": sre.TriggerLWLock,
}

// triggerFor is the investigation kind of a family; no family is the
// operator-started triage.
func triggerFor(family string) sre.TriggerKind {
	if kind, ok := families[family]; ok {
		return kind
	}
	return sre.TriggerOperator
}

var systemPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// decodeStrict reads one JSON object from at most limit bytes, refusing
// unknown fields and trailing data.
func decodeStrict(r io.Reader, limit int64, target any) error {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return invalidf("reading the body: %v", err)
	}
	if int64(len(raw)) > limit {
		return ErrTooLarge
	}
	if !utf8.Valid(raw) {
		return invalidf("the body is not valid UTF-8")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return invalidf("the body must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return invalidf("%v", err)
	}
	if dec.More() {
		return invalidf("trailing data after the JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalidf("trailing data after the JSON object")
	}
	return nil
}

// DecodeOpenRequest decodes an open request (Validate it before use).
func DecodeOpenRequest(r io.Reader) (OpenRequest, error) {
	var req OpenRequest
	return req, decodeStrict(r, MaxBodyBytes, &req)
}

// DecodeRemediationRequest decodes a remediation request; an empty body is
// a request without a note.
func DecodeRemediationRequest(r io.Reader) (RemediationRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return RemediationRequest{}, invalidf("reading the body: %v", err)
	}
	var req RemediationRequest
	if len(bytes.TrimSpace(raw)) == 0 {
		return req, nil
	}
	if err := decodeStrict(bytes.NewReader(raw), MaxBodyBytes, &req); err != nil {
		return req, err
	}
	return req, checkText("reason", req.Reason, false, maxReasonRunes, true)
}

// checkText bounds a text field; multiline allows newlines and tabs.
func checkText(name, value string, required bool, max int, multiline bool) error {
	switch {
	case required && strings.TrimSpace(value) == "":
		return invalidf("%s is required", name)
	case utf8.RuneCountInString(value) > max:
		return invalidf("%s longer than %d characters", name, max)
	}
	for _, r := range value {
		allowed := r == '\t' || (multiline && (r == '\n' || r == '\r'))
		if unicode.IsControl(r) && !allowed {
			return invalidf("%s contains control characters", name)
		}
	}
	return nil
}

// Validate checks the request against now.
func (r *OpenRequest) Validate(now time.Time) error {
	if r.Symptom == nil && r.Attach == nil {
		return invalidf("symptom or attach is required")
	}
	if r.Family != "" {
		if _, ok := families[r.Family]; !ok {
			return invalidf("unknown family %q", r.Family)
		}
	}
	for _, check := range []func() error{r.validateSymptom, r.validateAttach,
		r.validateExternal, func() error { return r.validateWindow(now) },
		func() error {
			return checkText("idempotency_key", r.IdempotencyKey, false,
				maxIdempotencyRunes, false)
		}} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (r *OpenRequest) validateSymptom() error {
	if r.Symptom == nil {
		return nil
	}
	if err := checkText("symptom.summary", r.Symptom.Summary, true, maxSummaryRunes,
		false); err != nil {
		return err
	}
	return checkText("symptom.description", r.Symptom.Description, false,
		maxDescriptionRunes, true)
}

func (r *OpenRequest) validateAttach() error {
	a := r.Attach
	if a == nil {
		return nil
	}
	if (a.InvestigationID == "") == (a.IncidentID == "") {
		return invalidf("attach takes exactly one of investigation_id or incident_id")
	}
	if a.InvestigationID != "" {
		id, err := sre.ParseUUID(a.InvestigationID)
		if err != nil {
			return invalidf("attach.investigation_id must be a UUID")
		}
		a.InvestigationID = string(id)
		return nil
	}
	return checkText("attach.incident_id", a.IncidentID, true, maxIncidentRunes, false)
}

func (r *OpenRequest) validateExternal() error {
	e := r.ExternalRef
	if e == nil {
		return nil
	}
	if !systemPattern.MatchString(e.System) {
		return invalidf("external_ref.system must match %s", systemPattern)
	}
	if err := checkText("external_ref.id", e.ID, true, maxExternalIDRunes, false); err != nil {
		return err
	}
	if e.URL == "" {
		return nil
	}
	u, err := url.Parse(e.URL)
	if err != nil || len(e.URL) > maxExternalURL || u.Host == "" ||
		(u.Scheme != "https" && u.Scheme != "http") {
		return invalidf("external_ref.url must be an http(s) URL")
	}
	return nil
}

func (r *OpenRequest) validateWindow(now time.Time) error {
	w := r.Window
	if w == nil {
		return nil
	}
	switch {
	case w.Start.IsZero():
		return invalidf("window.start is required")
	case w.Start.After(now.Add(windowFutureSkew)):
		return invalidf("window.start is in the future")
	case w.Start.Before(now.Add(-windowMaxAge)):
		return invalidf("window.start is older than 7 days")
	case w.End != nil && w.End.Before(w.Start):
		return invalidf("window.end is before window.start")
	case w.End != nil && w.End.After(now.Add(windowFutureSkew)):
		return invalidf("window.end is in the future")
	}
	return nil
}
