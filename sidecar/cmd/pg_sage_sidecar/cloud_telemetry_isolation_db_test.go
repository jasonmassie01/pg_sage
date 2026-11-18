package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// drill-01b on real PostgreSQL: the isolation findings reach sage.findings
// through upsertBackupFindings. State transitions: unknown (nothing) ->
// false (opens) -> unknown (stays open) -> true (resolves) -> unknown
// (stays resolved).

func TestUpsertIsolationDeletePathOnRealDatabase(t *testing.T) {
	pool := clusterPool(t, context.Background(), "backup_isolation")
	src := &capacitySource{}
	rt := backupRuntime(t, pool, src)
	rt.cfg.Backup.MinPITRHours = 24
	const ident = "backup:rds:us-east-1/orders"
	unknown := &cloudtel.BackupPosture{IsolationUnavailable: "AccessDenied"}
	no, yes := false, true

	pollBackup(t, rt, src, unknown)
	if got := openBackupFindings(t, pool, categoryBackupDeletePath); len(got) != 0 {
		t.Fatalf("unknown isolation opened %v", got)
	}
	pollBackup(t, rt, src, &cloudtel.BackupPosture{SurvivesDelete: &no,
		IsolationEvidence: "only automated backups"})
	got := openBackupFindings(t, pool, categoryBackupDeletePath)
	if f, ok := got[ident]; !ok || len(got) != 1 || f.severity != "warning" {
		t.Fatalf("automated only: open %v, want one warning %s", got, ident)
	}
	pollBackup(t, rt, src, unknown)
	if got := openBackupFindings(t, pool, categoryBackupDeletePath); len(got) != 1 {
		t.Fatalf("unknown isolation resolved the open finding: %v", got)
	}
	pollBackup(t, rt, src, &cloudtel.BackupPosture{SurvivesDelete: &yes,
		IsolationEvidence: "manual snapshot"})
	if got := openBackupFindings(t, pool, categoryBackupDeletePath); len(got) != 0 {
		t.Fatalf("a surviving backup left open %v", got)
	}
	pollBackup(t, rt, src, unknown)
	if got := openBackupFindings(t, pool, categoryBackupDeletePath); len(got) != 0 {
		t.Fatalf("unknown isolation reopened %v", got)
	}
}

func TestUpsertPITRWindowOnRealDatabase(t *testing.T) {
	pool := clusterPool(t, context.Background(), "backup_pitr_window")
	src := &capacitySource{}
	rt := backupRuntime(t, pool, src)
	rt.cfg.Backup.MinPITRHours = 24
	short, full := 23*time.Hour+59*time.Minute, 24*time.Hour
	pollBackup(t, rt, src, &cloudtel.BackupPosture{PITRWindow: &short})
	if got := openBackupFindings(t, pool, categoryPITRWindow); len(got) != 1 {
		t.Fatalf("23h59m window: open %v, want one", got)
	}
	// The other categories' findings are untouched by the window.
	if got := openBackupFindings(t, pool, categoryBackupDeletePath); len(got) != 0 {
		t.Fatalf("delete path opened without isolation evidence: %v", got)
	}
	pollBackup(t, rt, src, &cloudtel.BackupPosture{PITRWindow: &full})
	if got := openBackupFindings(t, pool, categoryPITRWindow); len(got) != 0 {
		t.Fatalf("24h window at the policy: open %v, want none", got)
	}
}
