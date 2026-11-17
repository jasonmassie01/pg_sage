package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// review-02a on a real server: bootstrap twice is clean and
// sage.review_candidate holds one row per (run, candidate) with a known
// status. No concurrent-writer test here: Bootstrap holds its advisory
// lock; the status race is candidate.TestStoreDB_ConcurrentMoves...
func TestReviewCandidatesMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "reviewcands")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'review_candidate'
		  AND column_name IN ('run_id', 'candidate_id', 'generator', 'dimension', 'kind',
		      'target', 'action_type', 'forward_sql', 'rollback_sql', 'evidence_ids',
		      'prediction', 'reversible', 'reason', 'approval', 'approval_reason',
		      'status', 'created_at', 'updated_at')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 18 {
		t.Fatalf("sage.review_candidate has %d of the 18 columns", cols)
	}
	checkCandidateConstraints(t, ctx, pool)
}

// checkCandidateConstraints stores one valid row and refuses each bad one;
// a deleted run takes its candidates.
func checkCandidateConstraints(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version) VALUES ('a', 'review', 'v') RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO sage.review_candidate (run_id, candidate_id, generator,
		dimension, kind, target, action_type, evidence_ids, status)
		VALUES ($1, $2, 'indexes', 'schema', 'unused', 'public.t_a', 'advice',
		'{index/idx/public.t_a}', $3)`
	id := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, insert, run, id, "proposed"); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, run, id, "proposed"); err == nil {
		t.Fatal("a second row for one (run, candidate) was stored")
	}
	for name, args := range map[string][]any{
		"unknown status": {run, strings.Repeat("b", 64), "done"},
		"short id":       {run, "abc", "proposed"},
		"unknown run":    {run + 99, strings.Repeat("c", 64), "proposed"},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s: the row was stored", name)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_candidate (run_id, candidate_id,
		generator, dimension, kind, target, action_type, evidence_ids)
		VALUES ($1, $2, 'g', 'd', 'k', 't', 'advice', '{}')`, run,
		strings.Repeat("d", 64)); err == nil {
		t.Error("a candidate without evidence was stored")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.review_run WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_candidate`).
		Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d candidates outlived their run (%v)", left, err)
	}
}

func TestReviewCandidatesStep_IsTheLastT6StepIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	// review-03a appended ddlReviewModelRuns after it (the last-step pin
	// moved there).
	if len(steps) < 2 || steps[len(steps)-2][0] != "ddlReviewCandidates" {
		t.Fatalf("T6 steps %v, want ddlReviewCandidates just before ddlReviewModelRuns",
			steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.review_candidate",
		"REFERENCES sage.review_run (id) ON DELETE CASCADE",
		"PRIMARY KEY (run_id, candidate_id)"} {
		if !strings.Contains(ddlReviewCandidates, want) {
			t.Errorf("ddlReviewCandidates lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlReviewCandidates) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
