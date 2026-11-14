package schema

import (
	"context"
	"strings"
	"testing"
)

// config-04c: the pure parts of the deferred index builder. No concurrent
// access tests here: these helpers are pure; the builder's concurrency is
// tested on a real server in deferred_index_db_test.go.

// Boundary: the threshold is inclusive; never-analyzed (-1) and empty (0)
// tables always build inline.
func TestAtOrAboveDeferral_Boundaries(t *testing.T) {
	cases := []struct {
		rows float64
		want bool
	}{
		{-1, false}, {0, false}, {1, false}, {999_999, false},
		{1_000_000, true}, {1_000_001, true}, {41_000_000, true},
	}
	for _, c := range cases {
		if got := atOrAboveDeferral(c.rows); got != c.want {
			t.Errorf("atOrAboveDeferral(%v) = %v, want %v", c.rows, got, c.want)
		}
	}
	if deferredIndexRows != 1_000_000 {
		t.Fatalf("deferredIndexRows = %d, want 1000000", deferredIndexRows)
	}
}

// The declared list starts with the eight ledger indexes, in order.
func TestDeferredIndexes_StartWithTheLedgerIndexes(t *testing.T) {
	if len(deferredIndexes) < len(ledgerIndexes) {
		t.Fatalf("deferredIndexes has %d entries, want at least the %d ledger indexes",
			len(deferredIndexes), len(ledgerIndexes))
	}
	for i, idx := range ledgerIndexes {
		if deferredIndexes[i] != idx {
			t.Errorf("deferredIndexes[%d] = %v, want %v", i, deferredIndexes[i], idx)
		}
	}
}

// The released step never changes: deferral is decided in Go, so the step
// SQL (and its pinned checksum) is what it was.
func TestDeferral_LeavesTheReleasedStepUnchanged(t *testing.T) {
	golden, _ := goldenChecksums(t)
	if got := stepChecksum(ddlDecisionLedger()); got != golden["ddlDecisionLedger"] {
		t.Fatalf("ddlDecisionLedger checksum %s, golden %s", got, golden["ddlDecisionLedger"])
	}
}

// The variant leaves out exactly the deferred indexes and keeps the
// columns and every other index.
func TestDeferredVariant_LeavesOutOnlyTheDeferredIndexes(t *testing.T) {
	step := migrationStep{name: "ddlDecisionLedger", sql: ddlDecisionLedger()}
	variant, ok := deferredVariant(step, []string{"idx_decision_created",
		"idx_decision_fingerprint"})
	if !ok {
		t.Fatal("deferredVariant refused a deferrable step")
	}
	if variant.name != step.name {
		t.Fatalf("variant name %q, want %q", variant.name, step.name)
	}
	if variant.sql == step.sql {
		t.Fatal("the variant is the released step")
	}
	for _, gone := range []string{"'idx_decision_created'", "'idx_decision_fingerprint'"} {
		if strings.Contains(variant.sql, gone) {
			t.Errorf("variant still builds %s", gone)
		}
	}
	for _, kept := range []string{"'idx_decision_action_log'", "'idx_verification_decision'",
		"'idx_schema_baseline_decision'", "ADD COLUMN IF NOT EXISTS fingerprint text"} {
		if !strings.Contains(variant.sql, kept) {
			t.Errorf("variant lost %s", kept)
		}
	}
	want := ledgerIndexBlock(ddlDecisionLedgerColumns, withoutIndexes(ledgerIndexes,
		[]string{"idx_decision_created", "idx_decision_fingerprint"}))
	if variant.sql != want {
		t.Fatalf("variant is not built by the step's generator:\n%s\nwant\n%s",
			variant.sql, want)
	}
}

// Nil/empty: deferring every index leaves a valid DO block without a loop
// (an empty VALUES list is a syntax error); deferring none is no variant.
func TestDeferredVariant_AllAndNone(t *testing.T) {
	step := migrationStep{name: "ddlDecisionLedger", sql: ddlDecisionLedger()}
	var all []string
	for _, idx := range ledgerIndexes {
		all = append(all, idx.name)
	}
	variant, ok := deferredVariant(step, all)
	if !ok {
		t.Fatal("deferredVariant refused to defer every index")
	}
	if strings.Contains(variant.sql, "VALUES") || strings.Contains(variant.sql, "FOR spec") {
		t.Fatalf("variant with no index left still has the loop:\n%s", variant.sql)
	}
	if !strings.Contains(variant.sql, "ADD COLUMN IF NOT EXISTS repeat_count") ||
		!strings.HasPrefix(variant.sql, "DO $$") || !strings.HasSuffix(variant.sql, "END $$;") {
		t.Fatalf("variant with no index left is not the columns block:\n%s", variant.sql)
	}
	if _, ok := deferredVariant(step, nil); ok {
		t.Fatal("deferredVariant built a variant with nothing deferred")
	}
	if _, ok := deferredVariant(migrationStep{name: "ddlUsersOAuth", sql: ddlUsersOAuth},
		[]string{"idx_decision_created"}); ok {
		t.Fatal("deferredVariant built a variant of a step that builds no deferred index")
	}
}

// The ledger neither fingerprints nor counts the deferred indexes as
// INVALID: an interrupted background build must not distrust it.
func TestLedgerSQL_LeavesOutTheDeferredIndexes(t *testing.T) {
	for _, idx := range deferredIndexes {
		lit := sqlLiteral(idx.name)
		if !strings.Contains(catalogFingerprintSQL, lit) {
			t.Errorf("catalogFingerprintSQL does not leave out %s", idx.name)
		}
		if !strings.Contains(catalogInvalidIndexSQL, lit) {
			t.Errorf("catalogInvalidIndexSQL does not leave out %s", idx.name)
		}
	}
	if !strings.Contains(catalogFingerprintSQL, "NOT IN") ||
		!strings.Contains(catalogInvalidIndexSQL, "NOT IN") {
		t.Fatal("the deferred names are not excluded with NOT IN")
	}
}

// Review issue 5: the WARN for an INVALID index only promises a rebuild
// when a migration step builds that index.
func TestInvalidIndexWarning_PromisesARebuildOnlyForStepIndexes(t *testing.T) {
	step := invalidIndexWarning("idx_findings_rule_id") // ddlFindingsAbsorbsSchemaFindings
	if !strings.Contains(step, "which rebuild it") || !strings.Contains(step,
		"idx_findings_rule_id") {
		t.Fatalf("step index WARN = %q", step)
	}
	other := invalidIndexWarning("idx_operator_made")
	if strings.Contains(other, "which rebuild it") {
		t.Fatalf("non-step index WARN promises a rebuild: %q", other)
	}
	if !strings.Contains(other, "no step rebuilds this index; drop it") ||
		!strings.Contains(other, "idx_operator_made") {
		t.Fatalf("non-step index WARN = %q", other)
	}
}

// Nil/empty: an empty declared list makes the builder return at once,
// without touching the (here nil) pool.
func TestBuildDeferredIndexes_EmptyListReturnsImmediately(t *testing.T) {
	saved := deferredIndexes
	deferredIndexes = nil
	t.Cleanup(func() { deferredIndexes = saved })
	report, err := BuildDeferredIndexes(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if len(report.Built)+len(report.Skipped)+len(report.Building)+len(report.Failed) != 0 {
		t.Fatalf("empty list report = %+v, want empty", report)
	}
}

// Invalid input: a nil pool with a non-empty list is an error, not a panic.
func TestBuildDeferredIndexes_NilPoolIsAnError(t *testing.T) {
	_, err := BuildDeferredIndexes(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("nil pool: err = %v, want one naming the pool", err)
	}
}

// The CONCURRENTLY statement is built from the declared definition.
func TestConcurrentIndexSQL(t *testing.T) {
	got := concurrentIndexSQL(ledgerIndexes[0])
	want := `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "idx_decision_fingerprint" ` +
		`ON sage.decision (fingerprint) WHERE fingerprint IS NOT NULL AND resolved_at IS NULL`
	if got != want {
		t.Fatalf("concurrentIndexSQL = %q\nwant %q", got, want)
	}
	got = concurrentIndexSQL(ledgerIndexes[1])
	want = `CREATE INDEX CONCURRENTLY IF NOT EXISTS "idx_decision_created" ` +
		`ON sage.decision (created_at)`
	if got != want {
		t.Fatalf("concurrentIndexSQL = %q\nwant %q", got, want)
	}
}
