package safetybench

import (
	"strings"
	"testing"
)

// No concurrent access tests: StatusIn and ScoreIncidents are pure functions
// of their arguments and share no state.

// matchedBy is a connected posture result that fired the given detectors.
func matchedBy(ids ...string) PostureResult {
	return PostureResult{ID: "PS-test", Connected: true, Expect: ids, MatchedDetectors: ids}
}

// missedBy is a connected posture result that expected the detectors and
// did not see them fire.
func missedBy(ids ...string) PostureResult {
	return PostureResult{ID: "PS-miss", Connected: true, Expect: ids, MissingDetectors: ids}
}

func TestIncidentStatusIn(t *testing.T) {
	both := []PostureResult{matchedBy("AP-03"), matchedBy("AP-04")}
	cases := []struct {
		name    string
		row     IncidentExpectation
		posture []PostureResult
		want    V0Status
	}{
		{"out of scope wins", IncidentExpectation{Expected: DispOutOfScope,
			PostureBacked: true, Detectors: []string{"AP-11"}},
			[]PostureResult{matchedBy("AP-11")}, StatusOutOfScope},
		{"posture-backed detection exercised", IncidentExpectation{Expected: DispDetected,
			PostureBacked: true, Detectors: []string{"AP-03", "AP-04"}}, both, StatusExercised},
		{"posture-backed future still exercises detection",
			IncidentExpectation{Expected: DispPrevented, PostureBacked: true,
				FutureRelease: true, Detectors: []string{"AP-03"}}, both, StatusExercised},
		{"one of two detectors matched is declared", IncidentExpectation{
			Expected: DispDetected, PostureBacked: true, Detectors: []string{"AP-03", "AP-04"}},
			[]PostureResult{matchedBy("AP-03")}, StatusDeclared},
		{"posture-backed with no detectors listed is declared", IncidentExpectation{
			Expected: DispDetected, PostureBacked: true}, both, StatusDeclared},
		{"unconnected match claims nothing", IncidentExpectation{Expected: DispDetected,
			PostureBacked: true, Detectors: []string{"AP-03"}},
			[]PostureResult{{ID: "PS-x", MatchedDetectors: []string{"AP-03"}}}, StatusDeclared},
		{"prevention without posture is future", IncidentExpectation{Expected: DispPrevented,
			FutureRelease: true}, both, StatusFuture},
		{"contained without posture is future", IncidentExpectation{Expected: DispContained,
			FutureRelease: true}, nil, StatusFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.StatusIn(tc.posture); got != tc.want {
				t.Fatalf("StatusIn() = %s, want %s", got, tc.want)
			}
		})
	}
}

// INC-01 is exercised only when the bench saw both of its detectors fire.
func TestINC01ExercisedWhenAP03AndAP04Matched(t *testing.T) {
	row := incident(t, "INC-01")
	got := row.StatusIn([]PostureResult{matchedBy("AP-03"), matchedBy("AP-04")})
	if got != StatusExercised {
		t.Fatalf("INC-01 with AP-03 and AP-04 matched = %s, want %s", got, StatusExercised)
	}
}

// With no posture results nothing is measured, so every posture-backed row
// that is not out of scope is declared, not exercised.
func TestNoPostureResultsDeclaresEveryPostureRow(t *testing.T) {
	for _, posture := range [][]PostureResult{nil, {}} {
		for _, r := range IncidentMapping() {
			got := r.StatusIn(posture)
			switch {
			case r.Expected == DispOutOfScope:
				if got != StatusOutOfScope {
					t.Errorf("%s = %s, want %s", r.ID, got, StatusOutOfScope)
				}
			case r.PostureBacked && got != StatusDeclared:
				t.Errorf("%s with no posture results = %s, want %s", r.ID, got,
					StatusDeclared)
			}
		}
	}
}

// INC-ORM's detectors (AP-13, AP-16) have no bench scenario: with every
// posture result the bench produces matched, the row is still declared.
func TestINCORMDeclaredWithoutAP13AP16(t *testing.T) {
	row := incident(t, "INC-ORM")
	all := []PostureResult{matchedBy("AP-01"), matchedBy("AP-02"), matchedBy("AP-03"),
		matchedBy("AP-04"), matchedBy("AP-05"), matchedBy("AP-07"), matchedBy("AP-10")}
	if got := row.StatusIn(all); got != StatusDeclared {
		t.Fatalf("INC-ORM without AP-13/AP-16 = %s, want %s", got, StatusDeclared)
	}
	if got := row.StatusIn([]PostureResult{matchedBy("AP-13")}); got != StatusDeclared {
		t.Fatalf("INC-ORM with only AP-13 = %s, want %s", got, StatusDeclared)
	}
	if got := row.StatusIn([]PostureResult{matchedBy("AP-13", "AP-16")}); got !=
		StatusExercised {
		t.Fatalf("INC-ORM with AP-13 and AP-16 = %s, want %s", got, StatusExercised)
	}
}

// A detector the bench expected and did not see fire is a miss: it
// downgrades the row to declared, even when another scenario matched it.
func TestMissedDetectorDowngradesINC01(t *testing.T) {
	row := incident(t, "INC-01")
	if got := row.StatusIn([]PostureResult{matchedBy("AP-03"), missedBy("AP-04")}); got !=
		StatusDeclared {
		t.Fatalf("INC-01 with AP-04 missed = %s, want %s", got, StatusDeclared)
	}
	mixed := []PostureResult{matchedBy("AP-03"), matchedBy("AP-04"), missedBy("AP-04")}
	if got := row.StatusIn(mixed); got != StatusDeclared {
		t.Fatalf("INC-01 with AP-04 matched once and missed once = %s, want %s", got,
			StatusDeclared)
	}
}

func TestScoreIncidents_Empty(t *testing.T) {
	s := ScoreIncidents(nil, nil)
	if s != (IncidentScore{}) {
		t.Fatalf("empty mapping must score zero, got %+v", s)
	}
}

func TestScoreIncidents_Mapping(t *testing.T) {
	rows := IncidentMapping()
	posture := []PostureResult{matchedBy("AP-03", "AP-04")}
	s := ScoreIncidents(rows, posture)
	want := IncidentScore{Total: 7, Exercised: 1, Declared: 1, OutOfScope: 2, Future: 3}
	if s != want {
		t.Fatalf("score = %+v, want %+v", s, want)
	}
	// SR-59: nothing scores as if it passed in v0 without a measurement.
	for _, r := range rows {
		if r.StatusIn(posture) == StatusExercised && !r.PostureBacked {
			t.Errorf("%s exercised in v0 without a posture backing", r.ID)
		}
	}
	none := ScoreIncidents(rows, nil)
	if none.Exercised != 0 || none.Declared != 2 {
		t.Fatalf("no posture: exercised %d declared %d, want 0 and 2", none.Exercised,
			none.Declared)
	}
}

func TestIncidentMapping_HasKnownRows(t *testing.T) {
	want := map[string]Disposition{
		"INC-01": DispDetected, "INC-04": DispPrevented, "INC-06": DispPrevented,
		"INC-15": DispContained, "INC-19": DispOutOfScope, "INC-20": DispOutOfScope,
		"INC-ORM": DispDetected,
	}
	got := map[string]Disposition{}
	for _, r := range IncidentMapping() {
		got[r.ID] = r.Expected
	}
	for id, disp := range want {
		if got[id] != disp {
			t.Errorf("%s expected %s, got %s", id, disp, got[id])
		}
	}
	// INC-04/06/15 need G1+, so they must be flagged future (not scored
	// as passing in v0).
	for _, id := range []string{"INC-04", "INC-06", "INC-15"} {
		if s := incident(t, id).StatusIn(nil); s != StatusFuture {
			t.Errorf("%s must be future_release in v0, got %s", id, s)
		}
	}
}

// INC-04 and INC-06 name what the bench measures today and what it does
// not, so a reader does not take a measured control for a missing one or
// the reverse.
func TestFutureRowsNameMeasuredAndUnmeasured(t *testing.T) {
	inc04 := incident(t, "INC-04").Why
	for _, want := range []string{"AQ-01", "AQ-03", "AQ-04", "pii", "secret", "taint",
		"Not measured"} {
		if !strings.Contains(inc04, want) {
			t.Errorf("INC-04 Why %q does not mention %q", inc04, want)
		}
	}
	if strings.Contains(inc04, "broker login (G1+)") {
		t.Errorf("INC-04 Why still lists the measured broker login as future: %q", inc04)
	}
	inc06 := incident(t, "INC-06").Why
	for _, want := range []string{"sc-create", "freeze", "Not measured"} {
		if !strings.Contains(inc06, want) {
			t.Errorf("INC-06 Why %q does not mention %q", inc06, want)
		}
	}
}

func incident(t *testing.T, id string) IncidentExpectation {
	t.Helper()
	for _, r := range IncidentMapping() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no incident row %s", id)
	return IncidentExpectation{}
}

func TestScoreDetectors(t *testing.T) {
	found := []PostureFinding{{DetectorID: "AP-03"}, {DetectorID: "AP-99"}}
	matched, missing := scoreDetectors([]string{"AP-03", "AP-04"}, found)
	if len(matched) != 1 || matched[0] != "AP-03" {
		t.Fatalf("matched = %v, want [AP-03]", matched)
	}
	if len(missing) != 1 || missing[0] != "AP-04" {
		t.Fatalf("missing = %v, want [AP-04]", missing)
	}
	// Empty expectations: nothing matched or missing.
	m, mi := scoreDetectors(nil, found)
	if len(m) != 0 || len(mi) != 0 {
		t.Fatalf("empty expect must yield nothing, got %v / %v", m, mi)
	}
}
