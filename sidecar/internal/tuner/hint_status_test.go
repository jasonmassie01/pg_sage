package tuner

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sage.query_hints reflects what happened to a hint (Phase 0 item 11): a
// proposal is 'proposed'; it becomes 'active' only once the executor has
// installed it in hint_plan.hints, and 'rolled_back' once the installed
// hint is gone again. An empty hint is never proposed or recorded.

const hintStatusQueryID int64 = 987650001

// No concurrent access tests: Tune and Revalidate serialize on t.mu and
// the reconciliation is a pair of idempotent UPDATE statements.

func requireHintStatusDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := requireTunerDB(t)
	exists, hasQueryID := hintTableLayout(t, pool)
	if exists && !hasQueryID {
		t.Skip("pg_hint_plan < 1.7: hint_plan.hints has no query_id column")
	}
	if !exists {
		for _, s := range []string{"CREATE SCHEMA IF NOT EXISTS hint_plan",
			`CREATE TABLE hint_plan.hints (id serial PRIMARY KEY, query_id bigint NOT NULL,
				application_name text NOT NULL, hints text NOT NULL)`} {
			if _, err := pool.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS hint_plan CASCADE")
		})
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.query_hints WHERE queryid = $1", hintStatusQueryID)
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM hint_plan.hints WHERE query_id = $1", hintStatusQueryID)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func hintStatuses(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT status FROM sage.query_hints WHERE queryid = $1 ORDER BY id",
		hintStatusQueryID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func readyTuner(pool *pgxpool.Pool) *Tuner {
	return New(pool, TunerConfig{}, &HintPlanAvailability{Available: true,
		HintTableReady: true}, noopLogFn)
}

func TestBuildFinding_ProposalIsNotActive(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	c := candidate{QueryID: hintStatusQueryID, Query: "SELECT 1"}
	f := tu.buildFinding(ctx, c, []PlanSymptom{{Kind: SymptomDiskSort}},
		`Set(work_mem "64MB")`, "title", "why", "", "")
	if f.RecommendedSQL == "" || f.RollbackSQL == "" {
		t.Fatalf("hint finding lost its SQL: %+v", f)
	}
	if got := hintStatuses(t, pool); len(got) != 1 || got[0] != "proposed" {
		t.Fatalf("statuses after proposal = %v, want [proposed]", got)
	}
	// A second proposal updates the proposed row; it never adds one.
	tu.buildFinding(ctx, c, []PlanSymptom{{Kind: SymptomDiskSort}},
		`Set(work_mem "128MB")`, "title", "why", "", "")
	var text string
	if err := pool.QueryRow(ctx, `SELECT hint_text FROM sage.query_hints
		WHERE queryid = $1`, hintStatusQueryID).Scan(&text); err != nil {
		t.Fatalf("one proposed row expected: %v", err)
	}
	if text != `Set(work_mem "128MB")` {
		t.Fatalf("hint_text = %q", text)
	}
}

func TestBuildFinding_EmptyHintIsNeverInserted(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	c := candidate{QueryID: hintStatusQueryID, Query: "SELECT 1"}
	f := tu.buildFinding(ctx, c, []PlanSymptom{{Kind: SymptomSortLimit}},
		"", "title", "add an index on the sort columns", "", "")
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("empty hint produced SQL: %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	if got := hintStatuses(t, pool); len(got) != 0 {
		t.Fatalf("empty hint recorded in query_hints: %v", got)
	}
	if f.Recommendation == "" || f.Category != "query_tuning" {
		t.Fatalf("informational finding lost: %+v", f)
	}
}

func TestReconcileHintStatuses_AppliedThenRolledBack(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	hint := `Set(work_mem "64MB")`
	tu.upsertQueryHint(ctx, hintStatusQueryID, hint, "disk_sort", "", "")
	tu.reconcileHintStatuses(ctx)
	if got := hintStatuses(t, pool); len(got) != 1 || got[0] != "proposed" {
		t.Fatalf("uninstalled proposal = %v, want [proposed]", got)
	}
	if _, err := pool.Exec(ctx, BuildInsertSQL(hintStatusQueryID, hint)); err != nil {
		t.Fatalf("install hint: %v", err)
	}
	tu.reconcileHintStatuses(ctx)
	if got := hintStatuses(t, pool); len(got) != 1 || got[0] != "active" {
		t.Fatalf("installed hint = %v, want [active]", got)
	}
	if _, err := pool.Exec(ctx, BuildDeleteSQL(hintStatusQueryID)); err != nil {
		t.Fatalf("remove hint: %v", err)
	}
	tu.reconcileHintStatuses(ctx)
	var status string
	var rolledBackAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, rolled_back_at FROM sage.query_hints
		WHERE queryid = $1`, hintStatusQueryID).Scan(&status, &rolledBackAt); err != nil {
		t.Fatal(err)
	}
	if status != "rolled_back" || rolledBackAt == nil {
		t.Fatalf("removed hint = %s at %v, want rolled_back with a timestamp",
			status, rolledBackAt)
	}
}

// A different hint installed for the query (e.g. by an operator) does not
// make this proposal active.
func TestReconcileHintStatuses_DifferentInstalledHintStaysProposed(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	tu.upsertQueryHint(ctx, hintStatusQueryID, `Set(work_mem "64MB")`, "disk_sort", "", "")
	if _, err := pool.Exec(ctx, BuildInsertSQL(hintStatusQueryID,
		"SeqScan(t)")); err != nil {
		t.Fatal(err)
	}
	tu.reconcileHintStatuses(ctx)
	if got := hintStatuses(t, pool); len(got) != 1 || got[0] != "proposed" {
		t.Fatalf("statuses = %v, want [proposed]", got)
	}
}

// Without a ready hint table nothing can be applied, so nothing moves.
func TestReconcileHintStatuses_NoHintTableIsNoop(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := New(pool, TunerConfig{}, &HintPlanAvailability{}, noopLogFn)
	tu.upsertQueryHint(ctx, hintStatusQueryID, `Set(work_mem "64MB")`, "disk_sort", "", "")
	if _, err := pool.Exec(ctx, BuildInsertSQL(hintStatusQueryID,
		`Set(work_mem "64MB")`)); err != nil {
		t.Fatal(err)
	}
	tu.reconcileHintStatuses(ctx)
	if got := hintStatuses(t, pool); len(got) != 1 || got[0] != "proposed" {
		t.Fatalf("statuses = %v, want [proposed]", got)
	}
	New(nil, TunerConfig{}, nil, noopLogFn).reconcileHintStatuses(ctx) // must not panic
}

// Proposed hints still count for the restart cooldown bootstrap.
func TestLoadActiveHints_IncludesProposed(t *testing.T) {
	pool, ctx := requireHintStatusDB(t)
	tu := readyTuner(pool)
	tu.upsertQueryHint(ctx, hintStatusQueryID, `Set(work_mem "64MB")`, "disk_sort", "", "")
	fresh := readyTuner(pool)
	fresh.loadActiveHints(ctx)
	if _, ok := fresh.recentlyTuned[hintStatusQueryID]; !ok {
		t.Fatal("proposed hint not in cooldown after restart")
	}
}
