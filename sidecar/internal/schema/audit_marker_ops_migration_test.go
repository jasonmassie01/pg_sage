package schema

import (
	"strings"
	"testing"
)

// audit-03b: the link table takes signed checkpoint (C) and prune (P)
// links. An install from before keeps its four-op check until this step
// widens it.

// No concurrent access tests here: the step runs under the bootstrap lock,
// which bootstrap_lock_test.go covers.

func TestAuditMarkerOpsStepIsGuarded(t *testing.T) {
	for _, want := range []string{"lock_timeout", "to_regclass('sage.audit_chain_link')",
		"'C'", "'P'", "NOT VALID", "IF NOT EXISTS"} {
		if !strings.Contains(ddlAuditChainMarkerOps, want) {
			t.Errorf("ddlAuditChainMarkerOps lacks %q", want)
		}
	}
}

// Integration: an install with the old check is widened, the marker index
// is created, and further bootstraps change nothing; other ops stay refused.
func TestAuditMarkerOpsMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, sql := range []string{
		`ALTER TABLE sage.audit_chain_link DROP CONSTRAINT audit_chain_link_op_check,
			ADD CONSTRAINT audit_chain_link_op_check CHECK (op IN ('I','U','D','T'))`,
		`DROP INDEX IF EXISTS sage.audit_chain_link_marker`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("simulate the old install: %v", err)
		}
	}
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'sage.audit_chain_link'::regclass
		AND conname = 'audit_chain_link_op_check'`).Scan(&def); err != nil {
		t.Fatalf("read the op check: %v", err)
	}
	for _, op := range []string{"'I'", "'U'", "'D'", "'T'", "'C'", "'P'"} {
		if !strings.Contains(def, op) {
			t.Fatalf("op check %s lacks %s", def, op)
		}
	}
	var idx string
	if err := pool.QueryRow(ctx, `SELECT coalesce(to_regclass(
		'sage.audit_chain_link_marker')::text, '')`).Scan(&idx); err != nil || idx == "" {
		t.Fatalf("marker index missing after bootstrap: %q, %v", idx, err)
	}
	ins := `INSERT INTO sage.audit_chain_link (chain, seq, row_id, op, v, sealed_hash,
		state, prev_hash, hash) VALUES ('schema_test', $1, 0, $2, 1, '', '', '', '')`
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.audit_chain_link WHERE chain = 'schema_test'`)
	})
	for i, op := range []string{"C", "P"} {
		if _, err := pool.Exec(ctx, ins, i+1, op); err != nil {
			t.Fatalf("op %s refused: %v", op, err)
		}
	}
	if _, err := pool.Exec(ctx, ins, 9, "X"); err == nil ||
		!strings.Contains(err.Error(), "audit_chain_link_op_check") {
		t.Fatalf("op X: err = %v, want the op check", err)
	}
}
