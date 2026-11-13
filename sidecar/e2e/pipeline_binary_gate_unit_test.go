//go:build e2e

package e2e

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// executor-10: the dump and CHECK-A11 are read by people triaging a red
// run, so their wording is pinned without a database.
// No concurrent access tests: both helpers are pure functions of their input.

func TestFormatSeedDecisions_NoRowsSaysNoDecision(t *testing.T) {
	got := formatSeedDecisions(seededObject{"A03", "pipe_bloat"}, nil)
	if got != "A03 pipe_bloat: no decision" {
		t.Fatalf("got %q, want the explicit no-decision line", got)
	}
	empty := formatSeedDecisions(seededObject{"A04", "pipe_stale"}, []decisionRow{})
	if empty != "A04 pipe_stale: no decision" {
		t.Fatalf("empty rows: got %q", empty)
	}
}

func TestFormatSeedDecisions_NamesVerdictReasonTierAndCount(t *testing.T) {
	rows := []decisionRow{
		{verdict: "queue_approval", reason: "sql_validation_degraded",
			riskTier: "safe", feature: "vacuum", count: 3, evidence: `{"k":1}`},
		{verdict: "execute", reason: "", riskTier: "safe", feature: "vacuum", count: 1},
	}
	got := formatSeedDecisions(seededObject{"A03", "pipe_bloat"}, rows)
	for _, want := range []string{
		"A03 pipe_bloat: ",
		"queue_approval/sql_validation_degraded tier=safe feature=vacuum x3",
		`evidence={"k":1}`,
		"execute/<no reason> tier=safe feature=vacuum x1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dump %q lacks %q", got, want)
		}
	}
	if strings.Count(got, " | ") != 1 {
		t.Errorf("dump %q: want the two groups separated once", got)
	}
}

func TestUnexplainedQueues(t *testing.T) {
	cases := []struct {
		name   string
		bySeed map[string][]decisionRow
		want   []string
	}{
		{"nil map", nil, nil},
		{"no rows", map[string][]decisionRow{"pipe_bloat": nil}, nil},
		{"known queue reason", map[string][]decisionRow{
			"pipe_bloat": {{verdict: "queue_approval", reason: "sql_validation_degraded"}},
		}, nil},
		{"empty reason", map[string][]decisionRow{
			"pipe_bloat": {{verdict: "queue_approval", reason: " "}},
		}, []string{"pipe_bloat:<no reason>"}},
		{"unknown reason", map[string][]decisionRow{
			"pipe_av": {{verdict: "queue_approval", reason: "mystery"}},
		}, []string{"pipe_av:mystery"}},
		{"read error fails the check", map[string][]decisionRow{
			"pipe_av": {{verdict: readErrorVerdict, reason: "boom"}},
		}, []string{"pipe_av:<read error> boom"}},
		{"other verdicts may carry any reason", map[string][]decisionRow{
			"pipe_stale": {{verdict: "blocked", reason: ""},
				{verdict: "execute", reason: "authorized"}},
		}, nil},
		{"one bad among good", map[string][]decisionRow{
			"pipe_stale": {{verdict: "queue_approval", reason: "approval_required"}},
			"pipe_dup_":  {{verdict: "queue_approval", reason: ""}},
		}, []string{"pipe_dup_:<no reason>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unexplainedQueues(c.bySeed)
			sort.Strings(got)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestPipelineSeedsCoverEveryMutatingCheck(t *testing.T) {
	want := map[string]string{"A01": "pipe_dup_", "A03": "pipe_bloat",
		"A04": "pipe_stale", "A05": "pipe_av", "A08": "pipe_invalid_idx"}
	got := map[string]string{}
	for _, seed := range mutatingSeeds {
		got[seed.check] = seed.name
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mutating seeds %v, want %v", got, want)
	}
	if len(pipelineSeeds) != len(mutatingSeeds)+2 {
		t.Fatalf("pipelineSeeds has %d entries, want the mutating ones plus A02 and A06",
			len(pipelineSeeds))
	}
}
