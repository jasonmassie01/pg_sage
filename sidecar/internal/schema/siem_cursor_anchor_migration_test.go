package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func siemCursorColumns(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT column_name, data_type
		FROM information_schema.columns WHERE table_schema = 'sage'
		AND table_name = 'siem_cursor' AND column_name IN ('hash', 'fork_to')`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[name] = typ
	}
	return out
}

// toPreUpgrade takes the cursor table back to before audit-01, with one
// cursor saved then.
func toPreUpgrade(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, sql := range []string{"DROP TRIGGER siem_cursor_hash_guard ON sage.siem_cursor",
		"ALTER TABLE sage.siem_cursor DROP COLUMN hash, DROP COLUMN fork_to",
		`INSERT INTO sage.siem_cursor (sink, source, chain, seq)
		VALUES ('soc', 'orders', 'action_log', 42)`} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func hashGuardTriggers(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_trigger
		WHERE tgrelid = 'sage.siem_cursor'::regclass
		AND tgname = 'siem_cursor_hash_guard'`).Scan(&n); err != nil {
		t.Fatalf("read triggers: %v", err)
	}
	return n
}

// audit-01: bootstrap adds the cursor's hash and fork_to columns on a
// fresh database and on one from before them, twice without error, and
// keeps existing cursors (their hash NULL until the exporter fills it).
func TestSIEMCursorAnchorMigration(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "schema_siem_anchor")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for run := 0; run < 2; run++ {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap %d: %v", run, err)
		}
	}
	cols := siemCursorColumns(t, pool)
	if cols["hash"] != "text" || cols["fork_to"] != "bigint" {
		t.Fatalf("fresh database columns = %v", cols)
	}
	if n := hashGuardTriggers(t, pool); n != 1 {
		t.Fatalf("fresh database has %d hash guard triggers, want 1", n)
	}
	toPreUpgrade(ctx, t, pool)
	for run := 0; run < 2; run++ {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("upgrade bootstrap %d: %v", run, err)
		}
	}
	if cols := siemCursorColumns(t, pool); len(cols) != 2 {
		t.Fatalf("upgraded database columns = %v", cols)
	}
	if n := hashGuardTriggers(t, pool); n != 1 {
		t.Fatalf("upgraded database has %d hash guard triggers, want 1", n)
	}
	var seq int64
	var hashNull, forkNull bool
	if err := pool.QueryRow(ctx, `SELECT seq, hash IS NULL, fork_to IS NULL
		FROM sage.siem_cursor WHERE sink = 'soc'`).Scan(&seq, &hashNull,
		&forkNull); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if seq != 42 || !hashNull || !forkNull {
		t.Fatalf("existing cursor after upgrade = %d hash-null=%v fork-null=%v", seq,
			hashNull, forkNull)
	}
}

// The column add waits at most lock_timeout for the table lock instead of
// hanging bootstrap behind a stuck session.
func TestSIEMCursorAnchorMigrationLockTimeout(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "schema_siem_anchor_lock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE sage.siem_cursor DROP COLUMN fork_to"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx, "LOCK TABLE sage.siem_cursor IN ACCESS SHARE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	start := time.Now()
	err = migrateSIEMCursorAnchor(ctx, conn)
	if err == nil {
		t.Fatalf("the migration took the table lock behind an open transaction")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("the migration waited %v; lock_timeout must bound it", d)
	}
	_ = holder.Rollback(ctx)
	if err := migrateSIEMCursorAnchor(ctx, conn); err != nil {
		t.Fatalf("migration once the lock is free: %v", err)
	}
	if cols := siemCursorColumns(t, pool); cols["fork_to"] != "bigint" {
		t.Fatalf("columns after retry = %v", cols)
	}
}
