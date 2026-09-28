package optimizer

import "testing"

// Before PG16 there is no EXPLAIN (GENERIC_PLAN). A normalized workload
// query ("... = $1") is prepared and executed with one NULL per parameter
// under force_generic_plan, so the plan never sees the NULLs as constants.
func TestPreparedExplainArgs(t *testing.T) {
	for n, want := range map[int]string{0: "", 1: "(NULL)", 3: "(NULL, NULL, NULL)"} {
		if got := preparedExplainArgs(n); got != want {
			t.Errorf("preparedExplainArgs(%d) = %q, want %q", n, got, want)
		}
	}
	if got := preparedExplainArgs(-1); got != "" {
		t.Errorf("negative parameter count must yield no argument list, got %q", got)
	}
}

// The pre-PG16 path runs on every server version, so exercise it directly:
// it must plan a normalized query, recover from a failed PREPARE without
// aborting the session transaction, and never leave the named statement on
// the pooled connection (prepared statements survive rollback).
func TestExplainPreparedPlansAndCleansUp(t *testing.T) {
	pool := hypopgSessionPool(t)
	s, err := openHypoPGSession(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.explainPrepared(t.Context(),
		"SELECT id FROM hypopg_session_test.items WHERE category=$1 AND id>$2")
	if err != nil || extractTotalCost(plan) <= 0 {
		t.Fatalf("prepared explain: cost=%f err=%v", extractTotalCost(plan), err)
	}
	if _, err := s.explainPrepared(t.Context(),
		"SELECT id FROM hypopg_session_test.no_such_table WHERE id=$1"); err == nil {
		t.Fatal("prepared explain of a missing table succeeded")
	}
	plan, err = s.explainPrepared(t.Context(),
		"SELECT id FROM hypopg_session_test.items WHERE category=$1")
	if err != nil || extractTotalCost(plan) <= 0 {
		t.Fatalf("session unusable after a failed PREPARE: err=%v", err)
	}
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var leaked int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_prepared_statements
		WHERE name = $1`, preparedWorkloadName).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("%d prepared workload statements leaked onto the pooled connection", leaked)
	}
	assertHypoPGSessionClean(t, pool)
}
