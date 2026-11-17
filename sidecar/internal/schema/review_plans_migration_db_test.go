package schema

import (
	"context"
	"strings"
	"testing"
)

// review-03b on a real server: bootstrap twice is clean and the review
// plan tables refuse what they must. No concurrent-writer test here:
// Bootstrap holds its advisory lock; the review plan store's concurrency is
// tested in internal/review/plan.

// stepIndex is the position of the step named name in steps, or -1.
func stepIndex(steps [][2]string, name string) int {
	for i, s := range steps {
		if s[0] == name {
			return i
		}
	}
	return -1
}

func TestReviewPlansStep_IsTheLastT6StepIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	if len(steps) == 0 || steps[len(steps)-1][0] != "ddlReviewPlans" ||
		steps[len(steps)-1][1] != ddlReviewPlans {
		t.Fatalf("T6 steps %v, want ddlReviewPlans last", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"CREATE TABLE IF NOT EXISTS sage.review_plan (",
		"CREATE TABLE IF NOT EXISTS sage.plan_step_origin",
		"CREATE TABLE IF NOT EXISTS sage.review_advice",
		"CREATE TABLE IF NOT EXISTS sage.review_model_authority",
		"REFERENCES sage.plan_step (id) ON DELETE CASCADE",
		"origin IN ('generator', 'model', 'person')", "UNIQUE (run_id, ordinal)"} {
		if !strings.Contains(ddlReviewPlans, want) {
			t.Errorf("ddlReviewPlans lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP TABLE"} {
		if strings.Contains(ddlReviewPlans, bad) {
			t.Errorf("ddlReviewPlans runs %q", bad)
		}
	}
}

func TestReviewPlansMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "reviewplans")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlReviewPlans'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlReviewPlans: %d (%v), want 1", ledger, err)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version) VALUES ('a', 'review', 'v') RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_plan (run_id, message)
		VALUES ($1, 'no changes proposed')`, run); err != nil {
		t.Fatalf("a plan-less review plan row was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_plan (run_id) VALUES ($1)`,
		run); err == nil {
		t.Fatal("a second review plan row for one run was accepted")
	}
	advice := `INSERT INTO sage.review_advice (run_id, ordinal, origin, kind, reason,
		detail) VALUES ($1, $2, $3, 'drop_unused_index', $4, $5)`
	if _, err := pool.Exec(ctx, advice, run, 1, "model", "no_template",
		"no template"); err != nil {
		t.Fatalf("valid advice was refused: %v", err)
	}
	for name, args := range map[string][]any{
		"duplicate ordinal": {run, 1, "model", "no_template", "x"},
		"unknown origin":    {run, 2, "agent", "no_template", "x"},
		"unknown reason":    {run, 3, "model", "because", "x"},
		"empty detail":      {run, 4, "model", "no_template", ""},
	} {
		if _, err := pool.Exec(ctx, advice, args...); err == nil {
			t.Errorf("%s: the advice row was accepted", name)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_model_authority (model,
		granted_by) VALUES ('m', '')`); err == nil {
		t.Error("an authority grant with no grantor was accepted")
	}
}
