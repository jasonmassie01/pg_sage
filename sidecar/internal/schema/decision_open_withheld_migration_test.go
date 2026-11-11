package schema

import (
	"strings"
	"testing"
)

// policy-07: idx_decision_open_withheld serves the decisions API's read
// of open withheld rows. It is built by the ledger index loop (catalog
// check first, INVALID rebuilt) under lock_timeout.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestDecisionOpenWithheldIndexIsRegisteredAndBounded(t *testing.T) {
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "idx_decision_open_withheld") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlDecisionOpenWithheldIndex" {
		t.Fatalf("steps naming the index = %v, want only ddlDecisionOpenWithheldIndex", found)
	}
	ddl := ddlDecisionOpenWithheldIndex()
	for _, want := range []string{"lock_timeout", "NOT i.indisvalid",
		"to_regclass('sage.' || spec.name) IS NULL",
		"(database_id, last_seen_at DESC)",
		"WHERE resolved_at IS NULL AND verdict <> ''execute''"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("the step lacks %q:\n%s", want, ddl)
		}
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server and leaves
// one valid partial index.
func TestDecisionOpenWithheldIndexBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "decision_open_withheld_idx")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	def := indexDefinition(t, pool, "idx_decision_open_withheld")
	if !strings.Contains(def, "(database_id, last_seen_at DESC)") ||
		!strings.Contains(def, "resolved_at IS NULL") ||
		!strings.Contains(def, "verdict <> 'execute'::text") || strings.Contains(def, "UNIQUE") {
		t.Fatalf("idx_decision_open_withheld = %s, want a partial index on open withheld rows",
			def)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid = 'sage.idx_decision_open_withheld'::regclass`).
		Scan(&valid); err != nil || !valid {
		t.Fatalf("idx_decision_open_withheld valid = %v (%v), want valid", valid, err)
	}
}

// An INVALID build is rebuilt by the next bootstrap.
func TestDecisionOpenWithheldIndexInvalidIsRebuilt(t *testing.T) {
	pool := freshPool(t, "decision_open_withheld_invalid")
	ctx := t.Context()
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = 'sage.idx_decision_open_withheld'::regclass`); err != nil {
		t.Skipf("cannot mark the index INVALID (needs superuser): %v", err)
	}
	bootstrapWithRetry(t, ctx, pool)
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index
		WHERE indexrelid = 'sage.idx_decision_open_withheld'::regclass`).
		Scan(&valid); err != nil || !valid {
		t.Fatalf("after re-bootstrap valid = %v (%v), want rebuilt", valid, err)
	}
}
