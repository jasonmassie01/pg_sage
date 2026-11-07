package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func readStopRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	var value, by string
	if err := pool.QueryRow(ctx, `SELECT value, COALESCE(updated_by, '')
		FROM sage.config WHERE key = 'emergency_stop'`).Scan(&value, &by); err != nil {
		t.Fatalf("read emergency_stop row: %v", err)
	}
	return value, by
}

func stopAuditCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE key = 'emergency_stop'`).Scan(&n); err != nil {
		t.Fatalf("count emergency_stop audit rows: %v", err)
	}
	return n
}

// executor-06: bootstrap installs sage.emergency_stop(stopped, actor) on a
// fresh server, twice cleanly, and the function upserts the flag with one
// sage.config_audit row per call (stop, then resume).
func TestEmergencyStopFunctionUpsertsFlagWithAudit(t *testing.T) {
	pool := freshPool(t, "emergency_stop_fn")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for run := 1; run <= 2; run++ {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("Bootstrap run %d: %v", run, err)
		}
	}
	if got := countStatement(migrationStatements(), ddlEmergencyStopFunction); got != 1 {
		t.Fatalf("emergency stop function registered %d times, want once", got)
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE key = 'emergency_stop'`).Scan(&before); err != nil {
		t.Fatalf("count emergency_stop rows: %v", err)
	}
	audits := stopAuditCount(t, ctx, pool)
	if _, err := pool.Exec(ctx, `SELECT sage.emergency_stop(true, 'dba')`); err != nil {
		t.Fatalf("sage.emergency_stop(true): %v", err)
	}
	if value, by := readStopRow(t, ctx, pool); value != "true" || by != "dba" {
		t.Fatalf("after stop: value=%q by=%q (rows before: %d), want true by dba",
			value, by, before)
	}
	if got := stopAuditCount(t, ctx, pool); got != audits+1 {
		t.Fatalf("audit rows after stop = %d, want %d", got, audits+1)
	}
	if _, err := pool.Exec(ctx, `SELECT sage.emergency_stop(false, 'dba')`); err != nil {
		t.Fatalf("sage.emergency_stop(false): %v", err)
	}
	if value, _ := readStopRow(t, ctx, pool); value != "false" {
		t.Fatalf("after resume: value=%q, want false", value)
	}
	if got := stopAuditCount(t, ctx, pool); got != audits+2 {
		t.Fatalf("audit rows after resume = %d, want %d", got, audits+2)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE key = 'emergency_stop'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("emergency_stop rows = %d (%v), want one upserted row", rows, err)
	}
}
