package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// pg_stat_statements near its max and dominated by utility statements
// (dogfood lifeos, 2026-10-04: 4,859 of 5,000 entries, 3,701 of them
// pg_dump COPY ... TO stdout, 100 deallocations). The finding is
// deterministic, cites the counts and says what to change, with the
// restart caveat for pg_stat_statements.max.

func usageSnap(max int, u *collector.StatStatementsUsage) *collector.Snapshot {
	return &collector.Snapshot{
		Queries: make([]collector.QueryStats, 500), // the collector's top-N cap
		System:  collector.SystemStats{StatStatementsMax: max, StatStatements: u},
	}
}

func lifeosUsage() *collector.StatStatementsUsage {
	return &collector.StatStatementsUsage{Entries: 4859, Utility: 3760, CopyOut: 3701,
		Dealloc: 100, TrackUtility: "on", Classified: true}
}

func TestStatStatementsCapacity_UtilityDominatedRecommendsTrackUtilityOff(t *testing.T) {
	got := ruleStatStatementsCapacity(usageSnap(5000, lifeosUsage()), nil, testConfig(), nil)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.Severity != "critical" || f.Category != "stat_statements_pressure" {
		t.Fatalf("finding = %s/%s, want critical stat_statements_pressure", f.Severity,
			f.Category)
	}
	for _, want := range []string{"4859", "5000", "3701"} {
		if !strings.Contains(f.Title, want) {
			t.Errorf("title %q lacks %s", f.Title, want)
		}
	}
	rec := f.Recommendation
	for _, want := range []string{"pg_stat_statements.track_utility = off",
		"pg_stat_statements.max", "restart", "100 deallocations"} {
		if !strings.Contains(rec, want) {
			t.Errorf("recommendation %q lacks %q", rec, want)
		}
	}
	d := f.Detail
	if d["tracked_queries"] != 4859 || d["utility_statements"] != 3760 ||
		d["copy_to_stdout_statements"] != 3701 || d["deallocations"] != int64(100) ||
		d["track_utility"] != "on" {
		t.Fatalf("detail = %v, want the counts cited", d)
	}
	if f.RecommendedSQL != "" {
		t.Fatalf("recommended SQL %q: a settings change is advice, never auto-applied",
			f.RecommendedSQL)
	}
}

// The real entry count, not the collector's top-500 read, decides.
func TestStatStatementsCapacity_UsesTrueEntryCount(t *testing.T) {
	u := &collector.StatStatementsUsage{Entries: 4100, Utility: 100, TrackUtility: "on",
		Dealloc: 0, Classified: true}
	got := ruleStatStatementsCapacity(usageSnap(5000, u), nil, testConfig(), nil)
	if len(got) != 1 || got[0].Severity != "warning" || got[0].Detail["tracked_queries"] != 4100 {
		t.Fatalf("findings = %+v, want a warning at 4100 of 5000", got)
	}
	if strings.Contains(got[0].Recommendation, "track_utility") {
		t.Fatalf("recommendation %q: utility is 2%%, it must not blame utility statements",
			got[0].Recommendation)
	}
	if !strings.Contains(got[0].Recommendation, "restart") {
		t.Fatalf("recommendation %q lacks the restart caveat", got[0].Recommendation)
	}
}

func TestStatStatementsCapacity_Boundaries(t *testing.T) {
	cases := []struct {
		name            string
		entries, util   int
		wantFindings    int
		wantTrackAdvice bool
	}{
		{"just under 80%", 3999, 3999, 0, false},
		{"exactly 80%", 4000, 2000, 1, true}, // utility exactly half
		{"utility just under half", 4000, 1999, 1, false},
	}
	for _, c := range cases {
		u := &collector.StatStatementsUsage{Entries: c.entries, Utility: c.util,
			TrackUtility: "on", Classified: true}
		got := ruleStatStatementsCapacity(usageSnap(5000, u), nil, testConfig(), nil)
		if len(got) != c.wantFindings {
			t.Errorf("%s: %d findings, want %d", c.name, len(got), c.wantFindings)
			continue
		}
		if len(got) == 1 && strings.Contains(got[0].Recommendation,
			"track_utility = off") != c.wantTrackAdvice {
			t.Errorf("%s: recommendation %q, want track_utility advice %v", c.name,
				got[0].Recommendation, c.wantTrackAdvice)
		}
	}
}

// track_utility already off: only the max can help.
func TestStatStatementsCapacity_TrackUtilityAlreadyOff(t *testing.T) {
	u := lifeosUsage()
	u.TrackUtility = "off"
	got := ruleStatStatementsCapacity(usageSnap(5000, u), nil, testConfig(), nil)
	if len(got) != 1 || strings.Contains(got[0].Recommendation, "track_utility = off") {
		t.Fatalf("findings = %+v, want max advice only", got)
	}
}

// Unclassified usage (below the classification threshold when read) and
// an unknown dealloc count are not invented.
func TestStatStatementsCapacity_UnknownsStayUnknown(t *testing.T) {
	u := &collector.StatStatementsUsage{Entries: 4900, Dealloc: -1, TrackUtility: "on"}
	got := ruleStatStatementsCapacity(usageSnap(5000, u), nil, testConfig(), nil)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	if _, ok := got[0].Detail["deallocations"]; ok {
		t.Fatalf("detail %v invents a deallocation count", got[0].Detail)
	}
	if _, ok := got[0].Detail["utility_statements"]; ok {
		t.Fatalf("detail %v invents a utility count", got[0].Detail)
	}
	if strings.Contains(got[0].Recommendation, "deallocations") {
		t.Fatalf("recommendation %q cites unknown deallocations", got[0].Recommendation)
	}
}

// No usage read (older snapshots): the legacy count of collected queries.
func TestStatStatementsCapacity_NilUsageFallsBack(t *testing.T) {
	got := ruleStatStatementsCapacity(usageSnap(550, nil), nil, testConfig(), nil)
	if len(got) != 1 || got[0].Detail["tracked_queries"] != 500 {
		t.Fatalf("findings = %+v, want the legacy 500 of 550", got)
	}
}
