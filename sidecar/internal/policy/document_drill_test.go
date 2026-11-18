package policy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// drill-01a: the standing policy document's drill block sets each
// database's restore drill: on or off, its cadence, the clone kinds it may
// use, its time budget and an operator marker query. No concurrent access
// tests: parsing and marshalling are pure functions of their input.

func withDrill(t *testing.T, drill string) []byte {
	t.Helper()
	raw, err := MarshalDocument(StaffedProfile())
	if err != nil {
		t.Fatalf("marshal staffed: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode staffed: %v", err)
	}
	wire["drill"] = json.RawMessage(drill)
	out, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

func TestDrillPolicy_HappyPathParsesEveryField(t *testing.T) {
	doc, err := ParseDocument(withDrill(t, `{"enabled": false, "cadence_days": 7,
		"kinds": ["branch", "full_copy"], "max_duration_minutes": 90,
		"marker_query": "SELECT max(at) FROM app.heartbeat"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := doc.Drill.Resolved()
	if got.On() || got.CadenceDays != 7 || len(got.Kinds) != 2 || got.Kinds[1] != "full_copy" ||
		got.MaxDurationMinutes != 90 || got.MarkerQuery != "SELECT max(at) FROM app.heartbeat" {
		t.Fatalf("drill = %+v", got)
	}
}

func TestDrillPolicy_AbsentBlockIsTheDefaults(t *testing.T) {
	doc := StaffedProfile()
	got := doc.Drill.Resolved()
	if !got.On() || got.CadenceDays != DefaultDrillCadenceDays ||
		got.MaxDurationMinutes != DefaultDrillMaxDurationMinutes || got.Kinds != nil {
		t.Fatalf("defaults = %+v, want on, %d days, %d minutes and the default kinds", got,
			DefaultDrillCadenceDays, DefaultDrillMaxDurationMinutes)
	}
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"drill"`)) {
		t.Fatalf("a default drill block was written: %s", raw)
	}
}

func TestDrillPolicy_InvalidValuesAreRefusedWithTheFieldName(t *testing.T) {
	cases := map[string]string{
		`{"cadence_days": -1}`:                    "drill.cadence_days",
		`{"cadence_days": 0}`:                     "drill.cadence_days",
		`{"cadence_days": 366}`:                   "drill.cadence_days",
		`{"max_duration_minutes": 0}`:             "drill.max_duration_minutes",
		`{"max_duration_minutes": 1441}`:          "drill.max_duration_minutes",
		`{"kinds": ["schema_only"]}`:              "drill.kinds",
		`{"kinds": ["tape"]}`:                     "drill.kinds",
		`{"kinds": []}`:                           "drill.kinds",
		`{"marker_query": "DELETE FROM app.t"}`:   "drill.marker_query",
		`{"marker_query": "SELECT 1; SELECT 2"}`:  "drill.marker_query",
		`{"marker_query": "   "}`:                 "drill.marker_query",
		`{"cadence": 7}`:                          "unknown field",
		`{"marker_query": "` + longQuery() + `"}`: "drill.marker_query",
	}
	for block, want := range cases {
		_, err := ParseDocument(withDrill(t, block))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to name %s", block, err, want)
		}
	}
}

func longQuery() string { return "SELECT " + strings.Repeat("1", 2001) }

func TestDrillPolicy_BoundaryValuesAreAccepted(t *testing.T) {
	for _, block := range []string{`{"cadence_days": 1}`, `{"cadence_days": 365}`,
		`{"max_duration_minutes": 1}`, `{"max_duration_minutes": 1440}`,
		`{"marker_query": "with m as (select now() at) select at from m"}`} {
		if _, err := ParseDocument(withDrill(t, block)); err != nil {
			t.Errorf("%s: %v", block, err)
		}
	}
}

func TestDrillPolicy_RoundTripsThroughMarshal(t *testing.T) {
	doc, err := ParseDocument(withDrill(t, `{"cadence_days": 10, "kinds": ["branch"]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseDocument(raw)
	if err != nil {
		t.Fatalf("reparse %s: %v", raw, err)
	}
	if got := again.Drill.Resolved(); got.CadenceDays != 10 || len(got.Kinds) != 1 ||
		!got.On() {
		t.Fatalf("round trip = %+v", got)
	}
}
