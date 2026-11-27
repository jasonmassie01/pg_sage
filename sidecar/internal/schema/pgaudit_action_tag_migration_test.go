package schema

import (
	"strings"
	"testing"
)

// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

// audit-06: the action-tag index is registered after executor-04's step,
// bounded by lock_timeout and idempotent.
func TestPGAuditActionTagIndexIsRegisteredAndBounded(t *testing.T) {
	statements := migrationStatements()
	at := -1
	for i, s := range statements {
		if strings.Contains(s, "guard_pgaudit_events_action_tag") {
			if at >= 0 {
				t.Fatalf("the action-tag index is registered twice (%d and %d)", at, i)
			}
			at = i
		}
	}
	if at < 0 || at == 0 || statements[at-1] != ddlRolloutInstanceStatus {
		t.Fatalf("the action-tag index is at %d, want right after ddlRolloutInstanceStatus", at)
	}
	ddl := statements[at]
	if !strings.Contains(ddl, "lock_timeout") || !strings.Contains(ddl, "IF NOT EXISTS") {
		t.Fatalf("the action-tag step is not bounded and idempotent:\n%s", ddl)
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server and leaves one
// partial index on application for tagged records.
func TestPGAuditActionTagIndexBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "pgaudit_action_tag_idx")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	def := indexDefinition(t, pool, "guard_pgaudit_events_action_tag")
	if !strings.Contains(def, "(application)") ||
		!strings.Contains(def, "WHERE (correlated_by = 'action_tag'::text)") {
		t.Fatalf("guard_pgaudit_events_action_tag = %s, want a partial index on "+
			"application of tagged records", def)
	}
}
