package schema

import (
	"context"
	"strings"
	"testing"
)

// review-01b on a real server: bootstrap twice is clean, sage.review_view
// keeps one row per run and user (its primary key), refuses a view of a
// run that does not exist, and goes with its run (ON DELETE CASCADE).
// No concurrent-writer test here: two tabs racing on one row is
// api.TestReviewViewDB_TwoTabsGiveOneRowPerUser.
func TestReviewViewsMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "reviewviews")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var pk string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'sage.review_view'::regclass AND contype = 'p'`).Scan(&pk); err != nil {
		t.Fatalf("primary key of sage.review_view: %v", err)
	}
	if pk != "PRIMARY KEY (run_id, user_id)" {
		t.Fatalf("primary key = %q, want (run_id, user_id)", pk)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version, status, finished_at) VALUES ('a', 'grade', 'pg_sage-rubric/1', 'done',
		now()) RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	view := `INSERT INTO sage.review_view (run_id, user_id) VALUES ($1, $2)`
	if _, err := pool.Exec(ctx, view, run, 7); err != nil {
		t.Fatalf("first view: %v", err)
	}
	if _, err := pool.Exec(ctx, view, run, 7); err == nil {
		t.Fatal("a second view by one user of one run was stored")
	}
	if _, err := pool.Exec(ctx, view, run, 0); err == nil {
		t.Fatal("a view by user 0 was stored")
	}
	if _, err := pool.Exec(ctx, view, -1, 7); err == nil {
		t.Fatal("a view of a run that does not exist was stored")
	}
	var at bool
	if err := pool.QueryRow(ctx, `SELECT viewed_at IS NOT NULL FROM sage.review_view
		WHERE run_id = $1 AND user_id = 7`, run).Scan(&at); err != nil || !at {
		t.Fatalf("viewed_at defaulted = %v %v, want set by the server", at, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.review_run WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_view`).Scan(&left); err !=
		nil || left != 0 {
		t.Fatalf("views left after their run was deleted = %d %v, want 0", left, err)
	}
}

// review-04a and review-04b appended steps after this one, so the pin is
// its place: right after review-05a's ddlReviewGate, and after ddlReviewRuns,
// the table its rows reference.
func TestReviewViewsStep_IsPlacedIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	runs, at := stepIndex(steps, "ddlReviewRuns"), stepIndex(steps, "ddlReviewViews")
	if runs < 0 || at <= runs || steps[at-1][0] != "ddlReviewGate" {
		t.Fatalf("T6 steps = %v, want ddlReviewRuns, then ddlReviewGate and ddlReviewViews",
			steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.review_view",
		"REFERENCES sage.review_run (id) ON DELETE CASCADE",
		"PRIMARY KEY (run_id, user_id)"} {
		if !strings.Contains(ddlReviewViews, want) {
			t.Errorf("ddlReviewViews lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlReviewViews) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
