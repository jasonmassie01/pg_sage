package schema

import (
	"context"
	"strings"
	"testing"
)

// review-02b on a real server: bootstrap twice is clean, the operator
// column is a nullable jsonb that holds an object or nothing.
func TestReviewCandidateOperatorMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "reviewcandop")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var typ, nullable string
	if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
		FROM information_schema.columns WHERE table_schema = 'sage'
		  AND table_name = 'review_candidate' AND column_name = 'operator'`).
		Scan(&typ, &nullable); err != nil {
		t.Fatalf("read the operator column: %v", err)
	}
	if typ != "jsonb" || nullable != "YES" {
		t.Fatalf("operator column %s nullable=%s, want a nullable jsonb", typ, nullable)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version) VALUES ('db', 'review', 'v') RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO sage.review_candidate (run_id, candidate_id, generator, dimension,
		kind, target, action_type, evidence_ids, operator)
		VALUES ($1, $2, 'config', 'configuration', 'k', 't', 'operator', '{setting/x}',
		$3::jsonb)`
	id := func(c string) string { return strings.Repeat(c, 64) }
	if _, err := pool.Exec(ctx, insert, run, id("a"), `{"command": "run"}`); err != nil {
		t.Fatalf("an object was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, run, id("b"), nil); err != nil {
		t.Fatalf("NULL was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, run, id("c"), `["run"]`); err == nil {
		t.Fatal("an array was stored as the operator fields")
	}
}

func TestReviewCandidateOperatorStep_IsTheLastT6StepAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	if len(steps) == 0 || steps[len(steps)-1][0] != "ddlReviewCandidateOperator" {
		t.Fatalf("T6 steps end with %v, want ddlReviewCandidateOperator",
			steps[len(steps)-1])
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS operator jsonb"} {
		if !strings.Contains(ddlReviewCandidateOperator, want) {
			t.Errorf("ddlReviewCandidateOperator lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlReviewCandidateOperator) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
