package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// migration-01 integration: VACUUM FULL on an analysed 50k-row table,
// queued behind a reader's ACCESS SHARE lock, is measured on the real
// server and every measurement reaches the finding detail and the case
// preflight.
func TestFindingDetail_VacuumFullMeasuredOnRealServer(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	table := schema + ".fd_orders"
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s AS
		SELECT g AS id, md5(g::text) AS note FROM generate_series(1, 50000) g`,
		table)); err != nil {
		t.Fatalf("create table: %v", err)
	}
	analyzeTable(t, pool, ctx, schema, "fd_orders")
	holdAndQueueVacuumFull(t, ctx, table)

	inc, risk, err := newTestAdvisor(t, pool).AnalyzeRisk(ctx, "VACUUM FULL "+table)
	if err != nil || inc == nil || risk == nil {
		t.Fatalf("AnalyzeRisk = %v, %v, %v", inc, risk, err)
	}
	finding, ok := FindingFromIncident(4242, "VACUUM FULL "+table, inc, risk)
	if !ok {
		t.Fatal("FindingFromIncident returned ok=false")
	}
	detail := roundTrip(t, finding.Detail)
	assertMeasuredVacuumFull(t, detail, schema)
	report := projectPreflight(t, finding, detail)
	for _, check := range report.Checks {
		if check.Status == "unknown" {
			t.Errorf("preflight check %s is unknown: %s", check.Name, check.Detail)
		}
	}
}

func assertMeasuredVacuumFull(t *testing.T, d map[string]any, schema string) {
	t.Helper()
	if size, _ := d["table_size_bytes"].(float64); size <= 0 {
		t.Errorf("table_size_bytes = %#v, want > 0", d["table_size_bytes"])
	}
	if rows, _ := d["estimated_rows"].(float64); rows < 45000 || rows > 55000 {
		t.Errorf("estimated_rows = %#v, want about 50000", d["estimated_rows"])
	}
	if locks, _ := d["pending_locks"].(float64); locks < 1 {
		t.Errorf("pending_locks = %#v, want >= 1", d["pending_locks"])
	}
	if d["lock_level"] != "ACCESS EXCLUSIVE" || d["requires_rewrite"] != true ||
		d["stats_known"] != true || d["affected_table"] != schema+".fd_orders" {
		t.Errorf("lock_level=%#v requires_rewrite=%#v stats_known=%#v table=%#v",
			d["lock_level"], d["requires_rewrite"], d["stats_known"], d["affected_table"])
	}
	at, err := time.Parse(time.RFC3339, fmt.Sprint(d["measured_at"]))
	if err != nil || time.Since(at) > time.Minute {
		t.Errorf("measured_at = %#v (%v), want a recent RFC 3339 time", d["measured_at"], err)
	}
}

// roundTrip passes the detail through JSON, as the findings table does.
func roundTrip(t *testing.T, detail map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return out
}

func projectPreflight(
	t *testing.T, f MigrationSafetyFinding, detail map[string]any,
) *cases.DDLPreflightReport {
	t.Helper()
	c := cases.ProjectFinding(cases.SourceFinding{
		ID: "fd-1", DatabaseName: "test_db", Category: SafetyFindingCategory,
		Severity: cases.Severity(f.Severity), ObjectType: f.ObjectType,
		ObjectIdentifier: f.ObjectIdentifier, RuleID: f.RuleID, Title: f.Title,
		RecommendedSQL: f.RecommendedSQL, Detail: detail, ObservedAt: time.Now(),
	}, nil) // nil: every change is a script-only ddl_preflight (cases-04)
	for _, candidate := range c.ActionCandidates {
		if candidate.DDLPreflight != nil {
			return candidate.DDLPreflight
		}
	}
	t.Fatalf("no candidate carries a DDL preflight: %+v", c.ActionCandidates)
	return nil
}

// holdAndQueueVacuumFull holds ACCESS SHARE on table in an open
// transaction and starts VACUUM FULL behind it, returning once the
// VACUUM waits for its lock, with the VACUUM's backend pid. Both
// sessions end when the test ends.
func holdAndQueueVacuumFull(t *testing.T, ctx context.Context, table string) uint32 {
	t.Helper()
	blocker, waiter := connectTest(t, ctx), connectTest(t, ctx)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+table+" LIMIT 1"); err != nil {
		t.Fatalf("blocker read: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(context.Background(), "VACUUM FULL "+table)
		done <- err
	}()
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("VACUUM FULL did not finish after the blocker ended")
		}
		_ = waiter.Close(context.Background())
		_ = blocker.Close(context.Background())
	})
	awaitLockWait(t, ctx, blocker, waiter.PgConn().PID())
	return waiter.PgConn().PID()
}

func connectTest(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return conn
}

func awaitLockWait(t *testing.T, ctx context.Context, conn *pgx.Conn, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid = $1 AND wait_event_type = 'Lock')`, int(pid)).Scan(&waiting)
		if err != nil {
			t.Fatalf("poll wait state: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("VACUUM FULL (pid %d) never waited for its lock", pid)
}
