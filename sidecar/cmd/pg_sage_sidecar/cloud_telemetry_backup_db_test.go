package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-07 on real PostgreSQL: the provider's backup posture reaches
// sage.findings through upsertBackupFindings. A stale restorable time
// opens backup_stale, a fresh one resolves it, and an unknown one (the
// field absent) leaves the open finding open instead of resolving it.

type backupFinding struct {
	severity, source string
}

func openBackupFindings(t *testing.T, pool *pgxpool.Pool,
	category string) map[string]backupFinding {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT object_identifier, severity,
		coalesce(detail->>'source', '') FROM sage.findings
		WHERE category = $1 AND status = 'open'`, category)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]backupFinding{}
	for rows.Next() {
		var id string
		var f backupFinding
		if err := rows.Scan(&id, &f.severity, &f.source); err != nil {
			t.Fatal(err)
		}
		out[id] = f
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// backupRuntime is a database runtime whose cloud telemetry reports the
// given backup posture for rds:us-east-1/orders.
func backupRuntime(t *testing.T, pool *pgxpool.Pool,
	src *capacitySource) *databaseRuntime {
	t.Helper()
	cloud, err := cloudtel.NewRuntime(src, cloudtel.RuntimeOptions{Database: "orders"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	cfg := &config.Config{Backup: config.BackupConfig{MaxAgeHours: 26,
		MaxArchiveGapMinutes: 15, MinRetentionDays: 7}}
	return &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders", Pool: pool,
		Scope: "orders"}, ctx: context.Background(), cloud: cloud, cfg: cfg}
}

func pollBackup(t *testing.T, rt *databaseRuntime, src *capacitySource,
	b *cloudtel.BackupPosture) {
	t.Helper()
	src.mu.Lock()
	src.sample = cloudtel.Sample{Provider: "rds", Resource: "rds:us-east-1/orders",
		Backup: b}
	src.mu.Unlock()
	if err := rt.cloud.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if err := rt.upsertBackupFindings(context.Background()); err != nil {
		t.Fatalf("upsertBackupFindings: %v", err)
	}
}

func TestUpsertBackupFindingsOnRealDatabase(t *testing.T) {
	pool := clusterPool(t, context.Background(), "backup_findings")
	src := &capacitySource{}
	rt := backupRuntime(t, pool, src)
	const ident = "backup:rds:us-east-1/orders"
	stale := time.Now().Add(-60 * time.Hour)
	days := 3
	pollBackup(t, rt, src, &cloudtel.BackupPosture{LatestRestorableTime: &stale,
		RetentionDays: &days})
	got := openBackupFindings(t, pool, categoryBackupStale)
	if f, ok := got[ident]; !ok || len(got) != 1 || f.severity != "critical" ||
		f.source != cloudtel.FreshnessRestorableTime {
		t.Fatalf("after a 60 h old restorable time: %v, want one critical %s", got, ident)
	}
	if ret := openBackupFindings(t, pool, categoryBackupRetention); len(ret) != 1 {
		t.Fatalf("retention 3 < 7: open %v, want one finding", ret)
	}

	// Unknown freshness (no restorable time) is no verdict: still open.
	pollBackup(t, rt, src, &cloudtel.BackupPosture{RetentionDays: &days})
	if got := openBackupFindings(t, pool, categoryBackupStale); len(got) != 1 {
		t.Fatalf("unknown freshness resolved the open finding: %v", got)
	}

	// A fresh restorable time and a retention at the policy resolve both.
	fresh, atPolicy := time.Now().Add(-5*time.Minute), 7
	pollBackup(t, rt, src, &cloudtel.BackupPosture{LatestRestorableTime: &fresh,
		RetentionDays: &atPolicy})
	if got := openBackupFindings(t, pool, categoryBackupStale); len(got) != 0 {
		t.Fatalf("fresh backup: open %v, want none", got)
	}
	if ret := openBackupFindings(t, pool, categoryBackupRetention); len(ret) != 0 {
		t.Fatalf("retention at the policy: open %v, want none", ret)
	}
}

// A failed poll leaves no sample: nothing is evaluated, nothing resolved.
func TestUpsertBackupFindingsUnavailableTelemetryChangesNothing(t *testing.T) {
	pool := clusterPool(t, context.Background(), "backup_unavailable")
	src := &capacitySource{}
	rt := backupRuntime(t, pool, src)
	stale := time.Now().Add(-30 * time.Hour)
	pollBackup(t, rt, src, &cloudtel.BackupPosture{LatestRestorableTime: &stale})
	if got := openBackupFindings(t, pool, categoryBackupStale); len(got) != 1 {
		t.Fatalf("30 h old backup: open %v, want one", got)
	}
	rt.cloud = cloudtel.Unavailable("orders", "rds", "no AWS credentials")
	if err := rt.upsertBackupFindings(context.Background()); err != nil {
		t.Fatalf("upsertBackupFindings: %v", err)
	}
	if got := openBackupFindings(t, pool, categoryBackupStale); len(got) != 1 ||
		got["backup:rds:us-east-1/orders"].severity != "warning" {
		t.Fatalf("unavailable telemetry changed the open finding: %v", got)
	}
}
