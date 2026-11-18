package schema

import (
	"context"
	"strings"
	"testing"
)

// review-01a on a real server: bootstrap twice is clean, sage.review_run
// allows one running row per database and sage.review_grade holds one row
// per dimension of a run. No concurrent-writer test here: Bootstrap holds
// its advisory lock; the running-row race is rubric.TestStoreDB_TwoRuns...
func TestReviewRunsMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "reviewruns")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'review_run'
		  AND column_name IN ('id', 'database_name', 'kind', 'triggers', 'rubric_version',
		      'window_start', 'window_end', 'pack_hash', 'status', 'overall_grade',
		      'overall_score', 'coverage_pct', 'started_at', 'finished_at', 'error')`).
		Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 15 {
		t.Fatalf("sage.review_run has %d of the 15 columns", cols)
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'sage'
		AND indexname = 'review_run_one_running'`).Scan(&def); err != nil {
		t.Fatalf("running index: %v", err)
	}
	if !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "'running'") {
		t.Fatalf("running index = %s, want a partial unique index", def)
	}
	insert := `INSERT INTO sage.review_run (database_name, kind, rubric_version)
		VALUES ('a', 'grade', 'pg_sage-rubric/1')`
	if _, err := pool.Exec(ctx, insert); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert); err == nil {
		t.Fatal("a second running row for one database was stored")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version, status) VALUES ('a', 'audit', 'v', 'done')`); err == nil {
		t.Fatal("an unknown kind was stored")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_grade (run_id, dimension, letter,
		score, coverage, checks) VALUES (-1, 'schema', 'A', 1, 1, '[]')`); err == nil {
		t.Fatal("a grade row without its run was stored")
	}
}

func TestReviewRunsStep_IsTheLastT6StepIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	// fleetpol-01a appended ddlPolicySets after this step.
	if len(steps) < 2 || steps[len(steps)-2][0] != "ddlReviewRuns" {
		t.Fatalf("T6 steps %v, want ddlReviewRuns just before ddlPolicySets", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.review_run",
		"CREATE TABLE IF NOT EXISTS sage.review_grade",
		"CREATE UNIQUE INDEX IF NOT EXISTS review_run_one_running"} {
		if !strings.Contains(ddlReviewRuns, want) {
			t.Errorf("ddlReviewRuns lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlReviewRuns) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
