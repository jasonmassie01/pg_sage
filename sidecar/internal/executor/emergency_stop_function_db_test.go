package executor

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const sqlStopActor = "dba-executor06"

func sqlStopAudits(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT COALESCE(old_value, '<none>') || '->' || new_value
		FROM sage.config_audit
		WHERE key = 'emergency_stop' AND changed_by_actor = $1 ORDER BY id`, sqlStopActor)
	if err != nil {
		t.Fatalf("read config_audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan config_audit: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate config_audit: %v", err)
	}
	return out
}

// CHECK-03: the documented SQL stop works on a database whose stop was never
// toggled through the API (no emergency_stop row): sage.emergency_stop(true,
// 'dba') makes CheckEmergencyStop true with one audit row, and
// sage.emergency_stop(false, 'dba') clears it with a second.
func TestSQLEmergencyStopFunctionDrivesCheckEmergencyStop(t *testing.T) {
	pool, ctx := requireDB(t)
	reset := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.config
			WHERE key = 'emergency_stop'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.config_audit
			WHERE changed_by_actor = $1`, sqlStopActor)
	}
	reset()
	t.Cleanup(reset)
	if CheckEmergencyStop(ctx, pool) {
		t.Fatal("stopped before the SQL stop ran")
	}
	var returned bool
	if err := pool.QueryRow(ctx, `SELECT sage.emergency_stop(true, $1)`,
		sqlStopActor).Scan(&returned); err != nil {
		t.Fatalf("sage.emergency_stop(true): %v", err)
	}
	state, err := ReadEmergencyStop(ctx, pool)
	if err != nil || !returned || !state.Stopped || state.UpdatedBy != sqlStopActor ||
		!CheckEmergencyStop(ctx, pool) {
		t.Fatalf("after SQL stop: returned=%v state=%+v err=%v, want stopped by %s",
			returned, state, err, sqlStopActor)
	}
	if got := sqlStopAudits(t, ctx, pool); len(got) != 1 || got[0] != "<none>->true" {
		t.Fatalf("audit rows after stop = %v, want [<none>->true]", got)
	}
	if _, err := pool.Exec(ctx, `SELECT sage.emergency_stop(false, $1)`,
		sqlStopActor); err != nil {
		t.Fatalf("sage.emergency_stop(false): %v", err)
	}
	if CheckEmergencyStop(ctx, pool) {
		t.Fatal("still stopped after sage.emergency_stop(false)")
	}
	if got := sqlStopAudits(t, ctx, pool); len(got) != 2 || got[1] != "true->false" {
		t.Fatalf("audit rows after resume = %v, want a second true->false", got)
	}
}

// Invalid input: no actor, or a NULL state, is refused and writes nothing.
func TestSQLEmergencyStopFunctionRejectsMissingActor(t *testing.T) {
	pool, ctx := requireDB(t)
	_, _ = pool.Exec(ctx, `DELETE FROM sage.config WHERE key = 'emergency_stop'`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.config
			WHERE key = 'emergency_stop'`)
	})
	for _, call := range []string{
		`SELECT sage.emergency_stop(true, '')`,
		`SELECT sage.emergency_stop(true, '   ')`,
		`SELECT sage.emergency_stop(true, NULL)`,
		`SELECT sage.emergency_stop(NULL, 'dba')`,
	} {
		if _, err := pool.Exec(ctx, call); err == nil {
			t.Errorf("%s succeeded, want refused", call)
		}
	}
	state, err := ReadEmergencyStop(ctx, pool)
	if err != nil || state.Stopped || state.UpdatedBy != "" {
		t.Fatalf("after refused calls: state=%+v err=%v, want no row", state, err)
	}
}
