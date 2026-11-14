package schema

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-06c: sage.notification_log names the database and dedup key of
// each delivery. The step is one idempotent T4 step under lock_timeout;
// rows written before it keep NULL (shown as "unknown").
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestNotificationLogDatabaseStepIsRegisteredAndBounded(t *testing.T) {
	four := trancheFourMigrations()
	if len(four) < 2 || four[0][0] != "ddlNotificationManagedBy" ||
		four[1][0] != "ddlNotificationLogDatabase" {
		t.Fatalf("T4 steps = %v, want ddlNotificationLogDatabase after "+
			"ddlNotificationManagedBy", four)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ALTER TABLE sage.notification_log",
		"ADD COLUMN IF NOT EXISTS database text",
		"ADD COLUMN IF NOT EXISTS dedup_key text",
		"CREATE INDEX IF NOT EXISTS idx_notification_log_database_sent",
		"(database, sent_at DESC)"} {
		if !strings.Contains(ddlNotificationLogDatabase, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	if strings.Contains(ddlNotificationLogDatabase, "NOT NULL") {
		t.Error("database and dedup_key must stay nullable: older rows have neither")
	}
	for _, stmt := range classifyStep(ddlNotificationLogDatabase) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not prechecked; it would run on every boot", stmt.sql)
		}
	}
}

// assertNotificationLogShape checks database and dedup_key are nullable
// text columns and the (database, sent_at DESC) index exists.
func assertNotificationLogShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'notification_log'
		  AND column_name IN ('database', 'dedup_key')
		  AND data_type = 'text' AND is_nullable = 'YES'`).Scan(&cols); err != nil {
		t.Fatalf("read notification_log columns: %v", err)
	}
	if cols != 2 {
		t.Fatalf("%d nullable text columns of database/dedup_key, want 2", cols)
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(
		to_regclass('sage.idx_notification_log_database_sent'))`).Scan(&def); err != nil {
		t.Fatalf("read the database index: %v", err)
	}
	if !strings.Contains(def, "(database, sent_at DESC)") {
		t.Fatalf("index = %q, want one on (database, sent_at DESC)", def)
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server, the step
// itself re-runs on a database that already has the columns, and a row
// written without a database reads back NULL.
func TestNotificationLogDatabaseBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "notification_log_db")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for run := 1; run <= 2; run++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, ddlNotificationLogDatabase); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("re-run %d of the step on existing columns: %v", run, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit re-run %d: %v", run, err)
		}
	}
	assertNotificationLogShape(t, ctx, pool)
}

// Upgrade: on the v2.4.0 dump a delivery row written before the step
// keeps NULL database and dedup_key after two boots.
func TestNotificationLogDatabaseUpgradeFromV240(t *testing.T) {
	pool := newPool(t, "notification_log_v240")
	restoreDump(t, pool, v240SageDump)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var id int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_log
		(event, subject, status) VALUES ('finding_opened', 'a06c old row', 'sent')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert a v2.4.0 row: %v", err)
	}
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("upgrade boot %d: %v", run, err)
		}
	}
	assertNotificationLogShape(t, ctx, pool)
	var dbNull, keyNull bool
	if err := pool.QueryRow(ctx, `SELECT database IS NULL, dedup_key IS NULL
		FROM sage.notification_log WHERE id = $1`, id).Scan(&dbNull, &keyNull); err != nil {
		t.Fatalf("read the old row: %v", err)
	}
	if !dbNull || !keyNull {
		t.Fatalf("old row database NULL=%v dedup_key NULL=%v, want both NULL", dbNull, keyNull)
	}
}
