package earned

import "testing"

// Owner decision 2026-10-04 (PR #110): executed CREATE STATISTICS is
// labeled create_statistics in action_log (before, the executor's label
// fell through to "ddl", which no class claimed). A rolled-back statistics
// action without a recorded outcome class is still the statistics class.
func TestStatisticsActionLabelIsTheStatisticsClass(t *testing.T) {
	if got := classForActionLabel("create_statistics"); got != ClassStatistics {
		t.Fatalf("classForActionLabel(create_statistics) = %q, want %s", got, ClassStatistics)
	}
	if got := classForActionLabel("ddl"); got != "" {
		t.Fatalf("classForActionLabel(ddl) = %q, want none", got)
	}
}
