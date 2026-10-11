package safetybench

import "testing"

func TestIncidentStatus(t *testing.T) {
	cases := []struct {
		name string
		row  IncidentExpectation
		want V0Status
	}{
		{"out of scope wins", IncidentExpectation{Expected: DispOutOfScope,
			PostureBacked: true}, StatusOutOfScope},
		{"posture-backed detection exercised", IncidentExpectation{Expected: DispDetected,
			PostureBacked: true}, StatusExercised},
		{"posture-backed future still exercises detection",
			IncidentExpectation{Expected: DispPrevented, PostureBacked: true,
				FutureRelease: true}, StatusExercised},
		{"prevention without posture is future", IncidentExpectation{Expected: DispPrevented,
			FutureRelease: true}, StatusFuture},
		{"contained without posture is future", IncidentExpectation{Expected: DispContained,
			FutureRelease: true}, StatusFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.Status(); got != tc.want {
				t.Fatalf("Status() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestScoreIncidents_Empty(t *testing.T) {
	s := ScoreIncidents(nil)
	if s != (IncidentScore{}) {
		t.Fatalf("empty mapping must score zero, got %+v", s)
	}
}

func TestScoreIncidents_Mapping(t *testing.T) {
	rows := IncidentMapping()
	s := ScoreIncidents(rows)
	if s.Total != len(rows) {
		t.Fatalf("total = %d, want %d", s.Total, len(rows))
	}
	if s.Exercised+s.OutOfScope+s.Future != s.Total {
		t.Fatalf("status counts %d+%d+%d != total %d", s.Exercised, s.OutOfScope,
			s.Future, s.Total)
	}
	// No row counted as a bare pass: every non-out-of-scope, non-posture
	// row must be future. SR-59: nothing scores as if it passed in v0.
	for _, r := range rows {
		if r.Status() == StatusExercised && !r.PostureBacked {
			t.Errorf("%s exercised in v0 without a posture backing", r.ID)
		}
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
		if statusOf(id) != StatusFuture {
			t.Errorf("%s must be future_release in v0", id)
		}
	}
}

func statusOf(id string) V0Status {
	for _, r := range IncidentMapping() {
		if r.ID == id {
			return r.Status()
		}
	}
	return ""
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
