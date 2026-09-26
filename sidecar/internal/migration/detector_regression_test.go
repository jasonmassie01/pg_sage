package migration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// blockedDDL holds an ACCESS EXCLUSIVE lock on table in one session
// and starts ddl in a second session, which then waits (state
// 'active') until cleanup releases the lock.
type blockedDDL struct {
	holder  *pgx.Conn
	blocked *pgx.Conn
	done    chan error
	pid     int
}

func startBlockedDDL(
	t *testing.T, dsn, table, ddl string,
) *blockedDDL {
	t.Helper()
	ctx := context.Background()
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	blocked, err := pgx.Connect(ctx, dsn)
	if err != nil {
		_ = holder.Close(ctx)
		t.Fatalf("connect blocked: %v", err)
	}
	b := &blockedDDL{holder: holder, blocked: blocked,
		done: make(chan error, 1)}
	if err := blocked.QueryRow(ctx,
		"SELECT pg_backend_pid()").Scan(&b.pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	mustExec(t, holder, "BEGIN")
	mustExec(t, holder, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE")
	go func() {
		_, err := blocked.Exec(context.Background(), ddl)
		b.done <- err
	}()
	t.Cleanup(func() { b.release(t) })
	waitForLockWait(t, holder, b.pid)
	return b
}

func (b *blockedDDL) release(t *testing.T) {
	ctx := context.Background()
	_, _ = b.holder.Exec(ctx, "ROLLBACK")
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		t.Errorf("blocked DDL did not finish after lock release")
	}
	_ = b.blocked.Close(ctx)
	_ = b.holder.Close(ctx)
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func waitForLockWait(t *testing.T, conn *pgx.Conn, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := conn.QueryRow(context.Background(),
			`SELECT COALESCE(wait_event_type = 'Lock', false)
			   FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting)
		if err == nil && waiting {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d never waited on the lock", pid)
}

func fixtureDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("SAGE_TEST_DATABASE_URL")
	}
	return dsn
}

// TestIntegration_Detector_SeesBlockedDDL is the G7-B02 regression:
// with the PostgreSQL ARE `\b` (backspace) the activity query never
// matched any DDL.
func TestIntegration_Detector_SeesBlockedDDL(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	table := schema + ".orders"
	_, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id int)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	ddl := "ALTER TABLE " + table + " ADD COLUMN c int DEFAULT random()"
	blocked := startBlockedDDL(t, fixtureDSN(t), table, ddl)

	det := newTestDetector(t, pool, newTestAdvisor(t, pool))
	incidents, err := det.PollOnce(ctx)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if got := det.knownQueries[blocked.pid]; got != ddl {
		t.Fatalf("detector did not see blocked DDL; known=%v",
			det.knownQueries)
	}
	if len(incidents) == 0 {
		t.Fatal("PollOnce returned no incident for blocked rewrite DDL")
	}
	if incidents[0].SignalIDs[0] != "ddl_add_column_volatile_default" {
		t.Fatalf("SignalIDs = %v", incidents[0].SignalIDs)
	}
}

// TestIntegration_RiskAssessor_CountsActiveQueries is the second half
// of G7-B02: `\b<table>\b` never matched, so ActiveQueries was 0.
func TestIntegration_RiskAssessor_CountsActiveQueries(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	table := schema + ".invoices"
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	startBlockedDDL(t, fixtureDSN(t), table,
		"SELECT count(*) FROM "+table)

	ra := NewRiskAssessor(pool, testLogFn(t))
	risk, err := ra.Assess(ctx, DDLClassification{
		RuleID: "ddl_set_not_null", LockLevel: "ACCESS EXCLUSIVE",
		TableName: "invoices", SchemaName: schema,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if risk.ActiveQueries < 1 {
		t.Fatalf("ActiveQueries = %d, want >= 1", risk.ActiveQueries)
	}
	if risk.PendingLocks < 1 {
		t.Fatalf("PendingLocks = %d, want >= 1", risk.PendingLocks)
	}
}

// TestIntegration_RiskAssessor_EscapesTableRegex: a table name with
// regex metacharacters must not break or widen the activity pattern.
func TestIntegration_RiskAssessor_EscapesTableRegex(t *testing.T) {
	pool, ctx := requireDB(t)
	ra := NewRiskAssessor(pool, testLogFn(t))
	risk, err := ra.Assess(ctx, DDLClassification{
		RuleID: "ddl_set_not_null", LockLevel: "ACCESS EXCLUSIVE",
		TableName: "a(b", SchemaName: "public",
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if risk.ActiveQueries != 0 {
		t.Fatalf("ActiveQueries = %d for unmatched table", risk.ActiveQueries)
	}
}

// TestIntegration_Detector_IgnoresOtherDatabases is the G7-B17
// regression: pg_stat_activity is cluster-wide.
func TestIntegration_Detector_IgnoresOtherDatabases(t *testing.T) {
	pool, ctx := requireDB(t)
	otherDSN := createSiblingDatabase(t, pool)
	other, err := pgx.Connect(ctx, otherDSN)
	if err != nil {
		t.Fatalf("connect sibling: %v", err)
	}
	mustExec(t, other, "CREATE TABLE leak_t (id int)")
	_ = other.Close(ctx)
	blocked := startBlockedDDL(t, otherDSN, "leak_t",
		"ALTER TABLE leak_t ADD COLUMN c int DEFAULT random()")

	det := newTestDetector(t, pool, newTestAdvisor(t, pool))
	if _, err := det.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if _, seen := det.knownQueries[blocked.pid]; seen {
		t.Fatalf("detector analyzed DDL from another database")
	}
}

func createSiblingDatabase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var current string
	if err := pool.QueryRow(ctx,
		"SELECT current_database()").Scan(&current); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	name := fmt.Sprintf("pgsage_b17_%d_%08x", os.Getpid(),
		time.Now().UnixNano()&0xffffffff)
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create sibling database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return strings.Replace(fixtureDSN(t), "/"+current, "/"+name, 1)
}

// TestIntegration_Detector_IgnoresOwnBackends covers G7-B27's
// "pg_sage's own executor DDL is analyzed" leg.
func TestIntegration_Detector_IgnoresOwnBackends(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	table := schema + ".own_t"
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	dsn := fixtureDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	blocked := startBlockedDDL(t, dsn+sep+"application_name=pg_sage",
		table, "ALTER TABLE "+table+" ADD COLUMN c int DEFAULT random()")

	det := newTestDetector(t, pool, newTestAdvisor(t, pool))
	if _, err := det.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if _, seen := det.knownQueries[blocked.pid]; seen {
		t.Fatalf("detector analyzed pg_sage's own DDL")
	}
}
