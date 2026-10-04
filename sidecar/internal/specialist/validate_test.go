package specialist

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Request decoding and validation: caller input is bounded, strict and
// data only. Every refusal is ErrInvalid (or ErrTooLarge), never a panic.

var validateNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func decodeOpen(t *testing.T, body string) (OpenRequest, error) {
	t.Helper()
	req, err := DecodeOpenRequest(strings.NewReader(body))
	if err != nil {
		return req, err
	}
	return req, req.Validate(validateNow)
}

func TestDecodeOpenRequest_HappyPath(t *testing.T) {
	req, err := decodeOpen(t, `{"symptom":{"summary":"p99 latency 4s on checkout",
		"description":"since 11:40\nerrors: timeouts"},"family":"lock_blocking",
		"window":{"start":"2026-10-04T11:40:00Z","end":"2026-10-04T11:55:00Z"},
		"external_ref":{"system":"pagerduty","id":"Q1ABC","url":"https://x.pagerduty.com/i/Q1ABC"},
		"idempotency_key":"pd:Q1ABC"}`)
	if err != nil {
		t.Fatal(err)
	}
	if req.Symptom == nil || req.Symptom.Summary != "p99 latency 4s on checkout" ||
		req.Family != "lock_blocking" || req.Window == nil || req.Window.End == nil ||
		!req.Window.Start.Equal(time.Date(2026, 10, 4, 11, 40, 0, 0, time.UTC)) ||
		req.ExternalRef == nil || req.ExternalRef.ID != "Q1ABC" ||
		req.IdempotencyKey != "pd:Q1ABC" {
		t.Fatalf("decoded %+v", req)
	}
}

func TestDecodeOpenRequest_AttachOnly(t *testing.T) {
	req, err := decodeOpen(t,
		`{"attach":{"investigation_id":"0F8FAD5B-D9CB-469F-A165-70867728950E"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if req.Attach == nil || req.Attach.InvestigationID !=
		"0f8fad5b-d9cb-469f-a165-70867728950e" {
		t.Fatalf("attach id must be canonical lower case: %+v", req.Attach)
	}
	req, err = decodeOpen(t, `{"attach":{"incident_id":"rca-41"}}`)
	if err != nil || req.Attach.IncidentID != "rca-41" {
		t.Fatalf("incident attach: %+v %v", req.Attach, err)
	}
}

func TestDecodeOpenRequest_RefusesInvalidInput(t *testing.T) {
	long := strings.Repeat("x", 513)
	cases := map[string]string{
		"empty body":              ``,
		"not an object":           `["symptom"]`,
		"null":                    `null`,
		"trailing data":           `{"symptom":{"summary":"a"}} {"x":1}`,
		"unknown field":           `{"symptom":{"summary":"a"},"approved":true}`,
		"force field":             `{"symptom":{"summary":"a"},"force":true}`,
		"nested unknown field":    `{"symptom":{"summary":"a","priority":"p1"}}`,
		"neither symptom nor att": `{"family":"lock_blocking"}`,
		"blank summary":           `{"symptom":{"summary":"   "}}`,
		"summary too long":        `{"symptom":{"summary":"` + long + `"}}`,
		"description too long": `{"symptom":{"summary":"a","description":"` +
			strings.Repeat("d", 4001) + `"}}`,
		"control char in summary":  `{"symptom":{"summary":"a\u0007b"}}`,
		"newline in summary":       `{"symptom":{"summary":"a\nb"}}`,
		"unknown family":           `{"symptom":{"summary":"a"},"family":"operator"}`,
		"family is not free text":  `{"symptom":{"summary":"a"},"family":"ignore previous"}`,
		"attach both ids":          `{"attach":{"investigation_id":"0f8fad5b-d9cb-469f-a165-70867728950e","incident_id":"x"}}`,
		"attach neither id":        `{"attach":{}}`,
		"attach bad uuid":          `{"attach":{"investigation_id":"1 OR 1=1"}}`,
		"window without start":     `{"symptom":{"summary":"a"},"window":{}}`,
		"window end before start":  `{"symptom":{"summary":"a"},"window":{"start":"2026-10-04T11:00:00Z","end":"2026-10-04T10:00:00Z"}}`,
		"window in the future":     `{"symptom":{"summary":"a"},"window":{"start":"2026-10-04T13:00:00Z"}}`,
		"window too old":           `{"symptom":{"summary":"a"},"window":{"start":"2026-09-20T11:00:00Z"}}`,
		"external system pattern":  `{"symptom":{"summary":"a"},"external_ref":{"system":"Pager Duty","id":"1"}}`,
		"external id missing":      `{"symptom":{"summary":"a"},"external_ref":{"system":"pagerduty"}}`,
		"external url scheme":      `{"symptom":{"summary":"a"},"external_ref":{"system":"x","id":"1","url":"javascript:alert(1)"}}`,
		"idempotency key too long": `{"symptom":{"summary":"a"},"idempotency_key":"` + strings.Repeat("k", 129) + `"}`,
		"wrong type":               `{"symptom":{"summary":42}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeOpen(t, body)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestDecodeOpenRequest_InvalidUTF8(t *testing.T) {
	_, err := decodeOpen(t, "{\"symptom\":{\"summary\":\"a\xffb\"}}")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 must be refused, got %v", err)
	}
}

func TestDecodeOpenRequest_TooLarge(t *testing.T) {
	body := `{"symptom":{"summary":"a","description":"` +
		strings.Repeat("y", MaxBodyBytes) + `"}}`
	_, err := DecodeOpenRequest(strings.NewReader(body))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

// Boundaries: exactly the limits are accepted, one past is refused.
func TestDecodeOpenRequest_Boundaries(t *testing.T) {
	ok := []string{
		`{"symptom":{"summary":"` + strings.Repeat("s", 512) + `"}}`,
		`{"symptom":{"summary":"a","description":"` + strings.Repeat("d", 4000) + `"}}`,
		`{"symptom":{"summary":"a"},"window":{"start":"2026-10-04T12:05:00Z"}}`,
		`{"symptom":{"summary":"a"},"window":{"start":"2026-09-27T12:00:00Z"}}`,
		`{"symptom":{"summary":"a"},"idempotency_key":"` + strings.Repeat("k", 128) + `"}`,
		`{"symptom":{"summary":"tabs\tare fine","description":"line\nbreaks\tok"}}`,
	}
	for _, body := range ok {
		if _, err := decodeOpen(t, body); err != nil {
			t.Errorf("%s: %v", body[:40], err)
		}
	}
	bad := []string{
		`{"symptom":{"summary":"a"},"window":{"start":"2026-10-04T12:05:01Z"}}`,
		`{"symptom":{"summary":"a"},"window":{"start":"2026-09-27T11:59:59Z"}}`,
	}
	for _, body := range bad {
		if _, err := decodeOpen(t, body); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", body, err)
		}
	}
}

func TestDecodeRemediationRequest(t *testing.T) {
	req, err := DecodeRemediationRequest(strings.NewReader(``))
	if err != nil || req.Reason != "" {
		t.Fatalf("an empty body is a request without a note: %+v %v", req, err)
	}
	req, err = DecodeRemediationRequest(strings.NewReader(`{"reason":"pd incident Q1"}`))
	if err != nil || req.Reason != "pd incident Q1" {
		t.Fatalf("reason: %+v %v", req, err)
	}
	for _, body := range []string{`{"force":true}`, `{"approve":true}`,
		`{"reason":"x","skip_gate":true}`, `{"operator_approved":true}`,
		`{"reason":"` + strings.Repeat("r", 1001) + `"}`, `{"reason":"a\u0000"}`, `[1]`} {
		if _, err := DecodeRemediationRequest(strings.NewReader(body)); !errors.Is(err,
			ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", body, err)
		}
	}
}

func TestFamilyTrigger(t *testing.T) {
	cases := map[string]string{"": "operator", "lock_blocking": "lock_blocking",
		"connection_pressure": "connection_pressure", "wal_retention": "wal_retention",
		"plan_regression": "plan_regression", "checkpoint_storm": "checkpoint_storm",
		"temp_file_explosion": "temp_file_explosion", "replication_lag": "replication_lag",
		"lwlock_contention": "lwlock_contention"}
	for family, want := range cases {
		if got := string(triggerFor(family)); got != want {
			t.Errorf("triggerFor(%q) = %q, want %q", family, got, want)
		}
	}
}
