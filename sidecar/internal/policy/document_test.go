package policy

import (
	"strings"
	"testing"
)

// policy-04: window_start_margin_minutes is 0-720 and optional; a document
// without it keeps its meaning (margin 0).
//
// No concurrent access tests: parsing and validation are pure functions.

func TestValidateDocumentWindowStartMargin(t *testing.T) {
	for _, tc := range []struct {
		margin int64
		ok     bool
	}{{-1, false}, {0, true}, {1, true}, {720, true}, {721, false}} {
		doc := StaffedProfile()
		doc.WindowStartMarginMinutes = tc.margin
		err := ValidateDocument(doc)
		if tc.ok && err != nil {
			t.Fatalf("margin %d: unexpected error %v", tc.margin, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(),
			"window_start_margin_minutes")) {
			t.Fatalf("margin %d: error = %v, want one naming window_start_margin_minutes",
				tc.margin, err)
		}
	}
}

func TestDocumentWindowStartMarginRoundTrips(t *testing.T) {
	doc := StaffedProfile()
	doc.WindowStartMarginMinutes = 45
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatalf("MarshalDocument: %v", err)
	}
	if !strings.Contains(string(raw), `"window_start_margin_minutes":45`) {
		t.Fatalf("marshalled document lacks the margin: %s", raw)
	}
	parsed, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if parsed.WindowStartMarginMinutes != 45 {
		t.Fatalf("parsed margin = %d, want 45", parsed.WindowStartMarginMinutes)
	}
}

func TestDocumentWithoutMarginKeepsItsMeaning(t *testing.T) {
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("MarshalDocument: %v", err)
	}
	// A zero margin is not written, so existing documents keep their bytes
	// (and their stored SHA-256) when re-marshalled.
	if strings.Contains(string(raw), "window_start_margin_minutes") {
		t.Fatalf("a zero margin was written: %s", raw)
	}
	parsed, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	if parsed.WindowStartMarginMinutes != 0 {
		t.Fatalf("margin = %d, want 0 for a document without the field",
			parsed.WindowStartMarginMinutes)
	}
}

func TestParseDocumentRejectsOutOfRangeMargin(t *testing.T) {
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("MarshalDocument: %v", err)
	}
	for _, value := range []string{"-1", "721"} {
		bad := strings.Replace(string(raw), `{`, `{"window_start_margin_minutes":`+value+`,`, 1)
		if _, err := ParseDocument([]byte(bad)); err == nil {
			t.Fatalf("margin %s parsed; want a validation error", value)
		}
	}
	bad := strings.Replace(string(raw), `{`, `{"window_start_margin_minutes":"30",`, 1)
	if _, err := ParseDocument([]byte(bad)); err == nil {
		t.Fatal("a string margin parsed; want a type error")
	}
}
