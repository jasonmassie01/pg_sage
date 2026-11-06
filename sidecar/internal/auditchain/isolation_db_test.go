package auditchain_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// isolationDB bootstraps a fresh database, then gives the test role the
// isolation default level there. It returns the DSN: only pools opened
// from it afterwards see the default.
func isolationDB(t *testing.T, label, level string) string {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	boot := openPool(t, dsn, false)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, boot); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	boot.Close()
	testdb.SetIsolationDefault(t, dsn, level)
	return dsn
}

// openPool opens a pool on dsn, configured the way pg_sage's own pools
// are when pinned is set, or a plain pool (a session whose pin a
// transaction pooler lost) when not.
func openPool(t *testing.T, dsn string, pinned bool) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 16
	if pinned {
		selfmonitor.ConfigurePool(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// CHECK-21: under a repeatable read or serializable role default (and the
// read committed one), concurrent writers through pg_sage's pools lose no
// link. Before the pin they failed at COMMIT with 23505 on (chain, seq).
func TestConcurrentWritersLoseNoLinksUnderRoleDefault(t *testing.T) {
	for _, level := range []string{"repeatable read", "serializable", "read committed"} {
		t.Run(level, func(t *testing.T) {
			dsn := isolationDB(t, "chain_iso_concurrent", level)
			assertConcurrentWritesChain(t, openPool(t, dsn, true))
		})
	}
}

// requireIsolationError requires err to be the chain's named refusal.
func requireIsolationError(t *testing.T, err error, level string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want a PostgreSQL error with SQLSTATE 55000", err)
	}
	if pgErr.Code != "55000" {
		t.Fatalf("SQLSTATE = %s (%s), want 55000", pgErr.Code, pgErr.Message)
	}
	if !strings.Contains(pgErr.Message, "transaction_isolation") ||
		!strings.Contains(pgErr.Message, level) {
		t.Fatalf("message %q must name transaction_isolation and %q", pgErr.Message, level)
	}
	if !strings.Contains(pgErr.Hint, "read committed") {
		t.Fatalf("hint %q must name the fix (read committed)", pgErr.Hint)
	}
}

// CHECK-22: an explicit repeatable read or serializable transaction that
// writes a chained table is refused at commit with a named 55000, not a
// 23505 under contention, and leaves neither the row nor a link.
func TestExplicitNonReadCommittedWriteIsRefused(t *testing.T) {
	pool := freshDB(t, "chain_iso_explicit")
	ctx := context.Background()
	for _, iso := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			t.Fatalf("begin %s: %v", iso, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
			VALUES ('vacuum', 'VACUUM iso')`); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("insert under %s: %v", iso, err)
		}
		requireIsolationError(t, tx.Commit(ctx), strings.ToLower(string(iso)))
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = 'VACUUM iso'`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 || linkCount(t, pool, auditchain.ActionLog.Chain) != 0 {
		t.Fatalf("a refused write left rows=%d links=%d, want none", rows,
			linkCount(t, pool, auditchain.ActionLog.Chain))
	}
}

// READ COMMITTED, and READ UNCOMMITTED (which PostgreSQL runs as READ
// COMMITTED: a new snapshot per statement), still append normally.
func TestExplicitReadCommittedWriteAppends(t *testing.T) {
	pool := freshDB(t, "chain_iso_rc")
	ctx := context.Background()
	for _, iso := range []pgx.TxIsoLevel{pgx.ReadCommitted, pgx.ReadUncommitted} {
		err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: iso}, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
				VALUES ('vacuum', $1)`, "VACUUM "+string(iso))
			return err
		})
		if err != nil {
			t.Fatalf("write under %s: %v", iso, err)
		}
	}
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.Links != 2 || rep.HeadSeq != 2 {
		t.Fatalf("links=%d head=%d problems=%+v, want a clean chain of 2",
			rep.Links, rep.HeadSeq, rep.Problems)
	}
}

// A session that lost pg_sage's pin (a plain pool under a repeatable read
// role default) fails closed on its first chained write, uncontended,
// with the named error.
func TestUnpinnedSessionUnderRoleDefaultFailsClosed(t *testing.T) {
	pool := openPool(t, isolationDB(t, "chain_iso_unpinned", "repeatable read"), false)
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed) VALUES ('vacuum', 'VACUUM unpinned')`)
	requireIsolationError(t, err, "repeatable read")
	if n := linkCount(t, pool, auditchain.ActionLog.Chain); n != 0 {
		t.Fatalf("links = %d after a refused write, want 0", n)
	}
}

// The startup self-check sees a role default the session pin hides from
// SHOW, names the role, the database and where the default comes from,
// and says nothing when the default is read committed.
func TestIsolationWarningSeesThroughThePin(t *testing.T) {
	ctx := context.Background()
	pool := openPool(t, isolationDB(t, "chain_iso_warn", "repeatable read"), true)
	var role, db string
	if err := pool.QueryRow(ctx, "SELECT session_user, current_database()").
		Scan(&role, &db); err != nil {
		t.Fatalf("read role and database: %v", err)
	}
	warn, err := auditchain.IsolationWarning(ctx, pool)
	if err != nil {
		t.Fatalf("IsolationWarning: %v", err)
	}
	for _, want := range []string{"'repeatable read'", role, db,
		"role " + role + " in database " + db, "55000",
		"SET default_transaction_isolation = 'read committed'"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning %q is missing %q", warn, want)
		}
	}
	clean := openPool(t, isolationDB(t, "chain_iso_clean", "read committed"), true)
	if warn, err := auditchain.IsolationWarning(ctx, clean); err != nil || warn != "" {
		t.Fatalf("read committed default: warning=%q err=%v, want none", warn, err)
	}
}

// A database-wide default is named as the database's.
func TestIsolationWarningNamesADatabaseDefault(t *testing.T) {
	ctx := context.Background()
	dsn := isolationDB(t, "chain_iso_dbwide", "read committed")
	admin := openPool(t, dsn, false)
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	exec(t, admin, "ALTER DATABASE "+pgx.Identifier{db}.Sanitize()+
		" SET default_transaction_isolation = 'serializable'")
	// The role-in-database setting (read committed) outranks the database
	// one, so drop it to let the database default through.
	exec(t, admin, "ALTER ROLE CURRENT_USER IN DATABASE "+pgx.Identifier{db}.Sanitize()+
		" RESET default_transaction_isolation")
	warn, err := auditchain.IsolationWarning(ctx, openPool(t, dsn, true))
	if err != nil {
		t.Fatalf("IsolationWarning: %v", err)
	}
	if !strings.Contains(warn, "'serializable'") || !strings.Contains(warn, "database "+db) {
		t.Fatalf("warning %q must name serializable and database %s", warn, db)
	}
}

// Error propagation: a failed read is an error that says what it read,
// never a silent "no warning".
func TestIsolationWarningPropagatesErrors(t *testing.T) {
	pool := freshDB(t, "chain_iso_err")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	warn, err := auditchain.IsolationWarning(ctx, pool)
	if err == nil || warn != "" {
		t.Fatalf("canceled read: warning=%q err=%v, want an error and no warning", warn, err)
	}
	if !strings.Contains(err.Error(), "default_transaction_isolation") {
		t.Fatalf("err = %v, want it to name default_transaction_isolation", err)
	}
}

// The guard is part of the function body that every bootstrap replaces:
// a second bootstrap on the same database runs cleanly, keeps it, and
// leaves an existing chain intact.
func TestBootstrapTwiceKeepsTheIsolationGuard(t *testing.T) {
	pool := freshDB(t, "chain_iso_twice")
	insertAction(t, pool, "CREATE INDEX twice")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	var body string
	if err := pool.QueryRow(ctx, `SELECT prosrc FROM pg_proc
		WHERE oid = 'sage.audit_chain_append(text,bigint,text,integer,text,text)'::regprocedure`).
		Scan(&body); err != nil {
		t.Fatalf("read audit_chain_append: %v", err)
	}
	if !strings.Contains(body, "current_setting('transaction_isolation')") ||
		!strings.Contains(body, "55000") {
		t.Fatalf("audit_chain_append lost the isolation guard:\n%s", body)
	}
	rep := verify(t, pool, auditchain.ActionLog, auditchain.Window{})
	if !rep.OK() || rep.Links != 1 {
		t.Fatalf("after a second bootstrap: links=%d problems=%+v", rep.Links, rep.Problems)
	}
}
