package executor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// advisors-07 on real PostgreSQL: an approved database-scope
// idle_in_transaction_session_timeout applies, reads back from
// pg_db_role_setting, and rolls back to the prior database value.
func TestTimeoutScope_DatabaseApplyReadBackRollback(t *testing.T) {
	p, ctx := requireDB(t)
	var database string
	if err := p.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{database}.Sanitize()
	base := pgconf.DatabaseStmt{Database: quoted,
		SystemStmt: pgconf.SystemStmt{Name: "idle_in_transaction_session_timeout"}}
	original, err := captureDatabaseSetting(ctx, p, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), databaseRollbackSQL(base, original)) })
	e := New(p, config.DefaultConfig(), time.Time{}, nopLog)
	for _, prior := range []string{"", "2min"} {
		t.Run("prior_"+prior, func(t *testing.T) {
			timeoutRoundTrip(t, ctx, e, database, prior)
		})
	}
}

// timeoutRoundTrip sets the timeout's prior database value, applies 5min,
// reads it back and rolls back to the prior value.
func timeoutRoundTrip(t *testing.T, ctx context.Context, e *Executor, database,
	prior string) {
	t.Helper()
	p := e.pool
	quoted := pgx.Identifier{database}.Sanitize()
	setup := "ALTER DATABASE " + quoted + " RESET idle_in_transaction_session_timeout"
	if prior != "" {
		setup = "ALTER DATABASE " + quoted +
			" SET idle_in_transaction_session_timeout = '" + prior + "'"
	}
	if _, err := p.Exec(ctx, setup); err != nil {
		t.Fatal(err)
	}
	sql := "ALTER DATABASE " + quoted + " SET idle_in_transaction_session_timeout = '5min'"
	change, err := e.prepareConfigChange(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	if change == nil || change.kind != configKindDatabaseGUC {
		t.Fatalf("missing database config capture: %+v", change)
	}
	if _, err := p.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
	if got := dbRoleSetting(t, ctx, p, database); got !=
		"idle_in_transaction_session_timeout=5min" {
		t.Fatalf("pg_db_role_setting after apply = %q", got)
	}
	if state, note, _ := e.readBackConfig(ctx, change); state != readbackInEffect {
		t.Fatalf("read-back state=%s note=%s", state, note)
	}
	if _, err := executeRollbackSQL(ctx, p, change.rollbackSQL,
		RollbackMonitorConfig{}); err != nil {
		t.Fatal(err)
	}
	want := ""
	if prior != "" {
		want = "idle_in_transaction_session_timeout=" + prior
	}
	if after := dbRoleSetting(t, ctx, p, database); after != want {
		t.Fatalf("after rollback pg_db_role_setting = %q, want %q", after, want)
	}
}

// dbRoleSetting is the database-wide (no role) entry for the timeout.
func dbRoleSetting(t *testing.T, ctx context.Context, p interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, database string) string {
	t.Helper()
	var got string
	err := p.QueryRow(ctx, `SELECT COALESCE((SELECT c FROM pg_db_role_setting s
		JOIN pg_database d ON d.oid = s.setdatabase, unnest(s.setconfig) AS c
		WHERE d.datname = $1 AND s.setrole = 0
		  AND c LIKE 'idle\_in\_transaction\_session\_timeout=%'), '')`,
		database).Scan(&got)
	if err != nil {
		t.Fatalf("read pg_db_role_setting: %v", err)
	}
	return got
}
