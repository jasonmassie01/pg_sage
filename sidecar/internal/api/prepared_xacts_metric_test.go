package api

import "testing"

// collection-05: the collector persists prepared_xacts, so the snapshot
// API serves it, latest and history alike: a database's prepared
// transactions are bounded by max_prepared_transactions.
//
// No concurrent access tests: validateMetric and historyMetricProblem are
// pure functions of their argument.
func TestSnapshotMetric_PreparedXactsIsServed(t *testing.T) {
	if !validateMetric("prepared_xacts") {
		t.Fatal("validateMetric(prepared_xacts) = false, want a snapshot category")
	}
	if p := historyMetricProblem("prepared_xacts"); p != "" {
		t.Fatalf("history of prepared_xacts refused: %q", p)
	}
	for _, bad := range []string{"prepared_xact", "PREPARED_XACTS", "prepared_xacts;"} {
		if validateMetric(bad) {
			t.Errorf("validateMetric(%q) = true, want false", bad)
		}
	}
}
