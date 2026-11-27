package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// alerting-09 review: pgx reports an error raised while the query runs
// (permission denied, statement timeout, a dropped connection) through
// rows.Err, not through Query. Such a database must be named in
// failed_databases instead of reading as healthy with no alerts.

const alertLogNoReadRole = "sage_alert_log_noread"

func noReadAlertLogPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles
		               WHERE rolname = 'sage_alert_log_noread') THEN
			CREATE ROLE sage_alert_log_noread NOLOGIN;
		END IF; END $$`)
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, stmt := range []string{
		"GRANT USAGE ON SCHEMA sage TO " + alertLogNoReadRole,
		"GRANT SELECT ON sage.findings TO " + alertLogNoReadRole,
		"REVOKE ALL ON sage.alert_log FROM " + alertLogNoReadRole,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+alertLogNoReadRole)
		return err
	}
	noRead, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open no-read pool: %v", err)
	}
	t.Cleanup(noRead.Close)
	return noRead
}

func TestAlertLog_QueryErrorDuringReadIsAFailedDatabase(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, err := pool.Exec(ctx, `INSERT INTO sage.alert_log
		 (severity, channel, dedup_key, status) VALUES
		 ('warning', 'slack', 'k-ok', 'sent')`)
	if err != nil {
		t.Fatalf("insert alert: %v", err)
	}
	mgr := alertLogFleet(map[string]*pgxpool.Pool{
		"a": pool, "noread": noReadAlertLogPool(t),
	})

	code, body := getAlertLog(t, mgr, "")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (error %q)", code, body.Error)
	}
	if len(body.Alerts) != 1 || body.Alerts[0]["database_name"] != "a" {
		t.Fatalf("alerts: got %v, want the one row of a", body.Alerts)
	}
	if len(body.FailedDatabases) != 1 || body.FailedDatabases[0] != "noread" {
		t.Errorf("failed_databases: got %v, want [noread]", body.FailedDatabases)
	}
}

func TestAlertLog_QueryErrorOnOnlySelectedDatabaseIs503(t *testing.T) {
	mgr := alertLogFleet(map[string]*pgxpool.Pool{
		"noread": noReadAlertLogPool(t),
	})
	code, body := getAlertLog(t, mgr, "?database=noread")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (alerts %v)", code, body.Alerts)
	}
	if len(body.FailedDatabases) != 1 || body.FailedDatabases[0] != "noread" {
		t.Errorf("failed_databases: got %v, want [noread]", body.FailedDatabases)
	}
}
