package autonomy

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// Regression tests for G4-B03 (freeze blocker selection must be scoped to
// the current database and exclude protected backends) and G4-B38 (XID rate
// must not count read-only transactions or invent a first-tick rate).

func holdSnapshot(t *testing.T, pool *pgxpool.Pool, appName string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET application_name = '"+appName+"'"); err != nil {
		t.Fatalf("set application_name: %v", err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM pg_class"); err != nil {
		t.Fatalf("take snapshot: %v", err)
	}
	pid := int(conn.Conn().PgConn().PID())
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		_, _ = conn.Exec(context.Background(), "RESET application_name")
		conn.Release()
	})
	return pid
}

func TestOldestXminBlockerExcludesProtectedApplications(t *testing.T) {
	pool := requireAutonomyDB(t)
	protected := holdSnapshot(t, pool, "pg_dump")
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25)

	blocker, err := custodian.oldestXminBlocker(context.Background())

	if err != nil {
		t.Fatalf("oldestXminBlocker: %v", err)
	}
	if blocker != nil && blocker.PID == protected {
		t.Fatalf("pg_dump backend %d selected as cancellable xmin blocker", protected)
	}
}

func TestOldestXminBlockerCapturesBackendIdentity(t *testing.T) {
	pool := requireAutonomyDB(t)
	holder := holdSnapshot(t, pool, "xmin_holder_app")
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25)

	blocker, err := custodian.oldestXminBlocker(context.Background())

	if err != nil {
		t.Fatalf("oldestXminBlocker: %v", err)
	}
	if blocker == nil || blocker.PID != holder {
		t.Fatalf("blocker = %#v, want pid %d", blocker, holder)
	}
	if blocker.BackendStart.IsZero() || blocker.AppName != "xmin_holder_app" {
		t.Fatalf("blocker identity incomplete: %#v", blocker)
	}
}

func TestFreezeRatesNeedTwoSamples(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, _, known := freezeRates(freezeCounters{}, freezeCounters{
		at: start, nextXID: 1000, mxidAge: 50,
	}); known {
		t.Fatal("first sample produced a known rate")
	}
	xid, mxid, known := freezeRates(
		freezeCounters{at: start, nextXID: 1000, mxidAge: 50},
		freezeCounters{at: start.Add(time.Minute), nextXID: 1600, mxidAge: 170},
	)
	if !known || xid != 10 || mxid != 2 {
		t.Fatalf("rates = %v/%v/%v, want 10/2/true", xid, mxid, known)
	}
}

func TestFreezeProposalOmitsDeadlineWhenRateUnknown(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	proposal, err := custodian.scanResponseRow(adapterRow{values: []any{
		"public", "orders", int64(90), int64(100), int64(1), int64(100), 0.1,
	}}, freezeRateSample{xid: 1, mxid: 1, known: false}, nil, false)
	if err != nil {
		t.Fatalf("scanResponseRow: %v", err)
	}
	if proposal.SQL == "" || proposal.Deadline != nil {
		t.Fatalf("proposal = %#v, want freeze SQL without a deadline override", proposal)
	}
}

func TestBlockerResponseEvidenceCarriesIdentity(t *testing.T) {
	start := time.Now().Add(-time.Hour).UTC()
	response, err := freeze.PlanResponse(freeze.ResponseInput{
		Schema: "public", Table: "orders", Urgency: freeze.UrgencyRed,
		Blocker: &freeze.XminBlocker{
			PID: 42, BackendStart: start, QueryStart: start, AppName: "app",
			Query: "SELECT 1", State: "active",
		},
	})
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	if response.Evidence["backend_start"] != start.Format(time.RFC3339Nano) ||
		response.Evidence["app_name"] != "app" {
		t.Fatalf("blocker evidence = %#v", response.Evidence)
	}
}
