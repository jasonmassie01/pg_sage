package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// review-03a on a real server: bootstrap twice is clean, a model run is
// written once and never updated, and the usage ledger takes the review
// client's role. No concurrent-writer test here: Bootstrap holds its
// advisory lock, and a run's rows are written by one pass.

// review-03b appended ddlReviewPlans after this step and review-05a
// ddlReviewGate after that, so the pin is its place (just before
// ddlReviewPlans), not "last".
func TestReviewModelRunsStep_IsPlacedIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := stepIndex(steps, "ddlReviewModelRuns")
	if at < 0 || at+1 >= len(steps) || steps[at+1][0] != "ddlReviewPlans" {
		t.Fatalf("T6 steps %v, want ddlReviewModelRuns then ddlReviewPlans", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.review_model_run",
		"REFERENCES sage.review_run (id) ON DELETE CASCADE", "PRIMARY KEY (run_id, attempt)",
		"octet_length(raw_output) <= 262144", "CREATE OR REPLACE FUNCTION",
		"IF NOT EXISTS (SELECT 1 FROM pg_trigger", "'review'"} {
		if !strings.Contains(ddlReviewModelRuns, want) {
			t.Errorf("ddlReviewModelRuns lacks %q", want)
		}
	}
	if strings.Contains(ddlReviewModelRuns, "UPDATE sage.") {
		t.Error("the step updates rows")
	}
}

func TestReviewModelRunsMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "reviewmodelruns")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version) VALUES ('a', 'review', 'v') RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO sage.review_model_run (run_id, attempt, role, prompt_version,
		pack_hash, output_sha256, status, raw_output) VALUES ($1, $2, 'rank', 'v1', $3, $3,
		$4, $5)`
	hash := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, insert, run, 1, hash, "done", "{}"); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
	for name, args := range map[string][]any{
		"second row for one attempt": {run, 1, hash, "done", ""},
		"attempt 6":                  {run, 6, hash, "done", ""},
		"unknown status":             {run, 2, hash, "running", ""},
		"short hash":                 {run, 3, "abc", "done", ""},
		"raw output over 256 KiB":    {run, 4, hash, "done", strings.Repeat("x", 262145)},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	_, err := pool.Exec(ctx, `UPDATE sage.review_model_run SET status = 'failed'
		WHERE run_id = $1`, run)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE of a final run: %v, want the append-only refusal", err)
	}
	checkReviewUsageRole(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.review_run WHERE id = $1`, run); err != nil {
		t.Fatalf("delete the run: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_model_run`).Scan(
		&left); err != nil || left != 0 {
		t.Fatalf("%d model runs outlived their review run (%v)", left, err)
	}
}

// checkReviewUsageRole: the usage ledger takes the review role and still
// refuses an unknown one.
func checkReviewUsageRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage (day, database_key, role, tokens)
		VALUES (current_date, 'process', 'review', 5)`); err != nil {
		t.Fatalf("the usage ledger refused the review role: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage (day, database_key, role, tokens)
		VALUES (current_date, 'process', 'unknown', 5)`); err == nil {
		t.Fatal("the usage ledger took an unknown role")
	}
}
