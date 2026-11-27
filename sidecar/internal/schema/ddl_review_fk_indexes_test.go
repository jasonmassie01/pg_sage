package schema

import (
	"context"
	"strings"
	"testing"
)

// The Sage Review foreign-key indexes (T6 round 1, review-03b and
// review-04a). No concurrent-writer test: Bootstrap holds its advisory
// lock, and the step only creates indexes. No nil/empty input: the step is
// a constant.

func TestReviewForeignKeyIndexesStep_IsTheLastT6StepAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	last := len(steps) - 1
	if last < 1 || steps[last][0] != "ddlReviewForeignKeyIndexes" ||
		steps[last][1] != ddlReviewForeignKeyIndexes ||
		steps[last-1][0] != "ddlMultixactMembers" {
		t.Fatalf("T6 steps %v, want ddlReviewForeignKeyIndexes last, after "+
			"ddlMultixactMembers", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"CREATE INDEX IF NOT EXISTS plan_step_origin_run",
		"ON sage.plan_step_origin (run_id) WHERE run_id IS NOT NULL",
		"CREATE INDEX IF NOT EXISTS review_trigger_coalesced",
		"ON sage.review_trigger (coalesced_into) WHERE coalesced_into IS NOT NULL"} {
		if !strings.Contains(ddlReviewForeignKeyIndexes, want) {
			t.Errorf("ddlReviewForeignKeyIndexes lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE ", "DELETE ", "DROP ", "CONCURRENTLY"} {
		if strings.Contains(ddlReviewForeignKeyIndexes, bad) {
			t.Errorf("ddlReviewForeignKeyIndexes runs %q", bad)
		}
	}
	stmts := classifyStep(ddlReviewForeignKeyIndexes)
	if len(stmts) != 3 {
		t.Fatalf("classified %d statements, want 3: %v", len(stmts), stmts)
	}
	for _, stmt := range stmts {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

func TestReviewForeignKeyIndexes_BootstrapTwiceIndexesBothKeys(t *testing.T) {
	pool := freshSchemaPool(t, "reviewfkidx") // bootstraps twice
	ctx := context.Background()
	for name, want := range map[string]string{
		"plan_step_origin_run":     "(run_id) WHERE (run_id IS NOT NULL)",
		"review_trigger_coalesced": "(coalesced_into) WHERE (coalesced_into IS NOT NULL)",
	} {
		var def string
		var valid bool
		err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
			FROM pg_index i WHERE i.indexrelid = to_regclass('sage.' || $1)`,
			name).Scan(&def, &valid)
		if err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		if !valid || !strings.HasSuffix(def, want) {
			t.Errorf("index %s = %q valid %t, want suffix %q", name, def, valid, want)
		}
	}
}

// Boundary: a foreign-key check is an equality lookup, which the partial
// index must serve; the ON DELETE SET NULL action still clears the child.
func TestReviewTriggerCoalescedIndex_ServesTheKeyAndSetNullStillWorks(t *testing.T) {
	pool := freshSchemaPool(t, "reviewfkcoal")
	ctx := context.Background()
	var parent, child int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_trigger
		(database_name, kind, dedupe_key) VALUES ('orders', 'calendar', 'p')
		RETURNING id`).Scan(&parent); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_trigger
		(database_name, kind, dedupe_key, state, coalesced_into, done_at)
		VALUES ('orders', 'calendar', 'c', 'coalesced', $1, now()) RETURNING id`,
		parent).Scan(&child); err != nil {
		t.Fatalf("insert child: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	var plan strings.Builder
	rows, err := conn.Query(ctx, `EXPLAIN (COSTS OFF)
		SELECT 1 FROM sage.review_trigger WHERE coalesced_into = $1`, parent)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "review_trigger_coalesced") {
		t.Errorf("key lookup plan does not use review_trigger_coalesced:\n%s", plan.String())
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.review_trigger WHERE id = $1`,
		parent); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	var into *int64
	if err := pool.QueryRow(ctx, `SELECT coalesced_into FROM sage.review_trigger
		WHERE id = $1`, child).Scan(&into); err != nil {
		t.Fatalf("read child: %v", err)
	}
	if into != nil {
		t.Fatalf("child coalesced_into = %d after its parent was deleted, want NULL", *into)
	}
}
