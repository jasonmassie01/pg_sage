package policy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// plan-02b: the standing policy document's plans block bounds what one
// plan may concentrate (indexes per table, index bytes, restart-bound
// steps) and when plan steps may start (peak windows). No concurrent
// access tests: parsing and marshalling are pure functions of their input.

// withPlans is the staffed profile's wire form with a plans block.
func withPlans(t *testing.T, plans string) []byte {
	t.Helper()
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("marshal staffed: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode staffed: %v", err)
	}
	wire["plans"] = json.RawMessage(plans)
	out, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func TestPlanCaps_HappyPathParsesEveryField(t *testing.T) {
	doc, err := ParseDocument(withPlans(t, `{"max_new_indexes_per_table": 3,
		"max_new_index_bytes_pct": 20, "max_restart_bound_steps": 1,
		"peak_windows": ["weekdays 09:00-17:00 America/Chicago"]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := doc.Plans.Resolved()
	if got.MaxNewIndexesPerTable != 3 || got.MaxNewIndexBytesPct != 20 ||
		got.MaxRestartBoundSteps != 1 || len(got.PeakWindows) != 1 ||
		got.PeakWindows[0] != "weekdays 09:00-17:00 America/Chicago" {
		t.Fatalf("plans = %+v, want 3, 20, 1 and one peak window", got)
	}
}

func TestPlanCaps_InvalidValuesAreRefusedWithTheFieldName(t *testing.T) {
	cases := map[string]string{
		`{"max_new_indexes_per_table": 0}`:           "plans.max_new_indexes_per_table",
		`{"max_new_indexes_per_table": 11}`:          "plans.max_new_indexes_per_table",
		`{"max_new_indexes_per_table": -1}`:          "plans.max_new_indexes_per_table",
		`{"max_new_index_bytes_pct": 51}`:            "plans.max_new_index_bytes_pct",
		`{"max_new_index_bytes_pct": 0}`:             "plans.max_new_index_bytes_pct",
		`{"max_restart_bound_steps": 2}`:             "plans.max_restart_bound_steps",
		`{"max_restart_bound_steps": 0}`:             "plans.max_restart_bound_steps",
		`{"peak_windows": ["weekdays 25:00-26:00"]}`: "plans.peak_windows",
		`{"peak_windows": [""]}`:                     "plans.peak_windows",
		`{"max_new_index_bytes_pct": 10.5}`:          "max_new_index_bytes_pct",
		`{"unknown_cap": 1}`:                         "unknown_cap",
	}
	for plans, field := range cases {
		_, err := ParseDocument(withPlans(t, plans))
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("plans %s: err %v, want a refusal naming %s", plans, err, field)
		}
	}
}

// Boundary: the range ends are accepted.
func TestPlanCaps_RangeEndsAreAccepted(t *testing.T) {
	for _, plans := range []string{
		`{"max_new_indexes_per_table": 1, "max_new_index_bytes_pct": 1}`,
		`{"max_new_indexes_per_table": 10, "max_new_index_bytes_pct": 50}`,
	} {
		if _, err := ParseDocument(withPlans(t, plans)); err != nil {
			t.Errorf("plans %s refused: %v", plans, err)
		}
	}
}

// Nil/empty: no document at all, a document without plans, a null or empty
// plans block and an empty peak_windows list all give the defaults (2,
// 10%, 1, no peak window), never zero caps.
func TestPlanCaps_AbsentBlockGivesTheDefaults(t *testing.T) {
	want := PlanCaps{MaxNewIndexesPerTable: 2, MaxNewIndexBytesPct: 10,
		MaxRestartBoundSteps: 1}
	check := func(name string, got PlanCaps) {
		t.Helper()
		if got.MaxNewIndexesPerTable != want.MaxNewIndexesPerTable ||
			got.MaxNewIndexBytesPct != want.MaxNewIndexBytesPct ||
			got.MaxRestartBoundSteps != want.MaxRestartBoundSteps ||
			len(got.PeakWindows) != 0 {
			t.Errorf("%s: caps %+v, want the defaults %+v", name, got, want)
		}
	}
	check("DefaultPlanCaps", DefaultPlanCaps())
	check("no document", Document{}.Plans.Resolved())
	check("staffed", StaffedProfile().Plans.Resolved())
	raw, err := MarshalDocument(UnattendedProfile())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	check("stored without plans", doc.Plans.Resolved())
	for _, plans := range []string{`null`, `{}`, `{"peak_windows": []}`} {
		doc, err := ParseDocument(withPlans(t, plans))
		if err != nil {
			t.Fatalf("plans %s: %v", plans, err)
		}
		check("plans "+plans, doc.Plans.Resolved())
		if _, open := doc.Plans.PeakWindowAt(time.Now()); open {
			t.Errorf("plans %s: a peak window is open, want none", plans)
		}
	}
}

// A stored document without plans round-trips byte for byte, so its
// policy_doc_sha256 never changes; one with non-default plans keeps them.
func TestPlanCaps_DocumentWithoutPlansRoundTripsByteForByte(t *testing.T) {
	for _, base := range []Document{StaffedProfile(), UnattendedProfile()} {
		first, err := MarshalDocument(base)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytes.Contains(first, []byte(`"plans"`)) {
			t.Fatalf("default document writes plans: %s", first)
		}
		doc, err := ParseDocument(first)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		second, err := MarshalDocument(doc)
		if err != nil || !bytes.Equal(first, second) {
			t.Fatalf("round trip changed the bytes (%v):\n%s\n%s", err, first, second)
		}
	}
	doc, err := ParseDocument(withPlans(t, `{"max_new_indexes_per_table": 4,
		"peak_windows": ["weekdays 09:00-17:00"]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	again, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("reparse %s: %v", raw, err)
	}
	got := again.Plans.Resolved()
	if got.MaxNewIndexesPerTable != 4 || got.MaxNewIndexBytesPct != 10 ||
		len(got.PeakWindows) != 1 || !bytes.Contains(raw, []byte(`"plans"`)) {
		t.Fatalf("non-default plans did not round-trip: %s -> %+v", raw, got)
	}
	// Explicit defaults are the defaults: not written back.
	doc, err = ParseDocument(withPlans(t, `{"max_new_indexes_per_table": 2,
		"max_new_index_bytes_pct": 10, "max_restart_bound_steps": 1}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if raw, _ := MarshalDocument(doc); bytes.Contains(raw, []byte(`"plans"`)) {
		t.Fatalf("explicit defaults written back: %s", raw)
	}
}

// ValidateDocument refuses out-of-range caps on a document built in code;
// zero fields mean the defaults.
func TestPlanCaps_ValidateDocumentChecksRanges(t *testing.T) {
	bad := []PlanCaps{{MaxNewIndexesPerTable: 11}, {MaxNewIndexesPerTable: -2},
		{MaxNewIndexBytesPct: 51}, {MaxRestartBoundSteps: 2},
		{PeakWindows: []string{"not a window"}}}
	for _, caps := range bad {
		doc := StaffedProfile()
		doc.Plans = caps
		if err := ValidateDocument(doc); err == nil || !strings.Contains(err.Error(), "plans.") {
			t.Errorf("caps %+v: err %v, want a plans refusal", caps, err)
		}
	}
	doc := StaffedProfile()
	doc.Plans = PlanCaps{MaxNewIndexBytesPct: 25}
	if err := ValidateDocument(doc); err != nil {
		t.Fatalf("partial caps refused: %v", err)
	}
	if got := doc.Plans.Resolved(); got.MaxNewIndexesPerTable != 2 ||
		got.MaxNewIndexBytesPct != 25 || got.MaxRestartBoundSteps != 1 {
		t.Fatalf("resolved %+v, want 2, 25, 1", got)
	}
}

// Boundary: a step at the first minute of a peak window is inside it, at
// the minute it closes it is not.
func TestPlanCaps_PeakWindowBoundaries(t *testing.T) {
	caps := PlanCaps{PeakWindows: []string{"weekdays 09:00-17:00 UTC"}}
	monday := func(h, m int) time.Time { return time.Date(2026, 11, 16, h, m, 0, 0, time.UTC) }
	cases := []struct {
		at   time.Time
		open bool
	}{
		{monday(8, 59), false}, {monday(9, 0), true}, {monday(16, 59), true},
		{monday(17, 0), false}, {monday(9, 0).AddDate(0, 0, 5), false},
	}
	for _, c := range cases {
		window, open := caps.PeakWindowAt(c.at)
		if open != c.open || (open && window != "weekdays 09:00-17:00 UTC") {
			t.Errorf("at %v: (%q, %v), want open=%v", c.at, window, open, c.open)
		}
	}
}

// Boundary: the byte cap allows exactly the percentage of the database
// and refuses one byte over; a zero or unknown database size allows none.
func TestPlanCaps_IndexBytesBoundary(t *testing.T) {
	caps := PlanCaps{}
	if !caps.IndexBytesWithin(1000, 10000) {
		t.Fatal("exactly 10% refused")
	}
	if caps.IndexBytesWithin(1001, 10000) {
		t.Fatal("one byte over 10% allowed")
	}
	if caps.IndexBytesWithin(1, 0) || caps.IndexBytesWithin(0, -1) {
		t.Fatal("no database size must allow no new index bytes")
	}
	if !caps.IndexBytesWithin(0, 1) {
		t.Fatal("no new bytes on a database must pass")
	}
	huge := int64(1) << 61
	if !caps.IndexBytesWithin(huge/10, huge) {
		t.Fatal("large sizes overflowed the arithmetic")
	}
	if got := caps.IndexBytesCap(10000); got != 1000 {
		t.Fatalf("cap of 10000 bytes = %d, want 1000", got)
	}
}
