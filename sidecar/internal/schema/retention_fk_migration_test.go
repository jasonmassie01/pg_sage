package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fkDeleteAction returns pg_constraint.confdeltype for the FK on
// sage.<table>(<column>), or "" when no such FK exists.
func fkDeleteAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string,
) string {
	t.Helper()
	var action string
	err := pool.QueryRow(ctx, `SELECT con.confdeltype::text
		FROM pg_constraint con
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = ANY(con.conkey)
		WHERE con.contype = 'f'
		  AND con.conrelid = to_regclass('sage.' || $1)
		  AND a.attname = $2`, table, column).Scan(&action)
	if err != nil {
		t.Fatalf("read FK action for sage.%s(%s): %v", table, column, err)
	}
	return action
}

// G1-B12 / G7-B11: nullable audit references to purgeable rows must be
// ON DELETE SET NULL so retention can delete expired parents; the
// migration must also convert installs created with NO ACTION.
func TestBootstrap_RetentionForeignKeysSetNull(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	// Simulate an older install: restore the original NO ACTION constraint.
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.alert_log
		DROP CONSTRAINT IF EXISTS alert_log_finding_id_fkey,
		ADD CONSTRAINT alert_log_finding_id_fkey FOREIGN KEY (finding_id)
		REFERENCES sage.findings(id)`); err != nil {
		t.Fatalf("simulate legacy FK: %v", err)
	}
	for i := 0; i < 2; i++ { // second run proves idempotency
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("Bootstrap run %d: %v", i+1, err)
		}
	}
	want := [][2]string{
		{"alert_log", "finding_id"},
		{"findings", "action_log_id"},
		{"action_queue", "action_log_id"},
		{"decision", "action_log_id"},
		{"decision", "queue_id"},
		{"verification", "action_log_id"},
		{"schema_baseline", "last_authorized_action_id"},
		{"schema_baseline", "last_authorized_decision_id"},
		{"action_log", "decision_id"},
		{"action_log", "verification_id"},
	}
	for _, fk := range want {
		if got := fkDeleteAction(t, ctx, pool, fk[0], fk[1]); got != "n" {
			t.Errorf("sage.%s(%s) ON DELETE action = %q, want SET NULL (n)",
				fk[0], fk[1], got)
		}
	}
}
