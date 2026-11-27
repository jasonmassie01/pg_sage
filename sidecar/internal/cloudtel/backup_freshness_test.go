package cloudtel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// market-07: backup freshness from the providers. RDS reports the latest
// restorable time on the instance (Aurora on the cluster) at no extra
// call; Cloud SQL lists at most 5 backup runs, at most every 15 minutes.

// No concurrent access tests beyond the source's mutex: Collect runs on
// the runtime's poll goroutine; the cache is guarded like s.name.

func restorableXML(at time.Time) string {
	return "<LatestRestorableTime>" + at.UTC().Format(time.RFC3339) +
		"</LatestRestorableTime>"
}

func TestAWSCollectRecordsLatestRestorableTime(t *testing.T) {
	f := rdsFixture(t, true)
	restorable := awsNow.Add(-30 * time.Hour)
	f.instances["orders"] = withBackupXML(f.instances["orders"],
		"<BackupRetentionPeriod>7</BackupRetentionPeriod>"+restorableXML(restorable))
	b := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"})).Backup
	if b == nil || b.LatestRestorableTime == nil ||
		!b.LatestRestorableTime.Equal(restorable) {
		t.Fatalf("backup = %+v, want latest restorable %s", b, restorable)
	}
	got, source, ok := b.LastGood()
	if !ok || !got.Equal(restorable) || source != "latest_restorable_time" {
		t.Fatalf("LastGood = %s, %q, %v", got, source, ok)
	}
	if *b.RetentionDays != 7 {
		t.Fatalf("retention = %d, want 7 kept beside the freshness", *b.RetentionDays)
	}
}

// Nil/empty: a response without LatestRestorableTime is unknown freshness.
func TestAWSCollectWithoutRestorableTimeIsUnknown(t *testing.T) {
	f := rdsFixture(t, true)
	b := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"})).Backup
	if b == nil || b.LatestRestorableTime != nil {
		t.Fatalf("backup = %+v, want no restorable time", b)
	}
	if _, _, ok := b.LastGood(); ok {
		t.Fatal("LastGood known without a restorable time")
	}
	var none *BackupPosture
	if _, _, ok := none.LastGood(); ok {
		t.Fatal("nil posture has a last good backup")
	}
}

// Aurora keeps the restorable time on the cluster, read with the writer
// lookup the source already makes: through the cluster endpoint and
// through an instance endpoint.
func TestAuroraCollectReadsTheClusterRestorableTime(t *testing.T) {
	restorable := awsNow.Add(-5 * time.Minute)
	for _, opts := range []AWSOptions{{ClusterID: "shop"}, {InstanceID: "shop-1"}} {
		f := newFakeAWS(t, awsNow)
		f.instances["shop-1"] = instanceXML("shop-1", "db.r7g.xlarge", "aurora-postgresql",
			"shop-pg16", "in-sync", 1, 0, false, "shop")
		f.clusters["shop"] = strings.Replace(clusterXML("shop", "shop-1"), "</DBCluster>",
			restorableXML(restorable)+"</DBCluster>", 1)
		f.metric("shop-1", "CPUUtilization", time.Minute, 12)
		f.metric("shop-1", "FreeableMemory", time.Minute, 20*gib)
		b := mustCollect(t, f.source(t, opts)).Backup
		if b == nil || b.LatestRestorableTime == nil ||
			!b.LatestRestorableTime.Equal(restorable) {
			t.Fatalf("%+v: backup = %+v, want the cluster's restorable time", opts, b)
		}
	}
}

func backupRun(status string, end time.Time) map[string]any {
	return map[string]any{"status": status, "type": "AUTOMATED",
		"endTime": end.UTC().Format(time.RFC3339Nano)}
}

func TestGCPCollectReadsTheLatestSuccessfulBackupRun(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	settings := f.instances["main"]["settings"].(map[string]any)
	settings["backupConfiguration"] = map[string]any{"enabled": true,
		"pointInTimeRecoveryEnabled": true, "transactionLogRetentionDays": 7}
	good := gcpNow.Add(-20 * time.Hour)
	f.backupRuns["main"] = []map[string]any{
		{"status": "RUNNING", "type": "AUTOMATED"},
		backupRun("FAILED", gcpNow.Add(-2*time.Hour)),
		backupRun("SUCCESSFUL", good),
		backupRun("SUCCESSFUL", gcpNow.Add(-44*time.Hour)),
	}
	b := mustCollect(t, f.source(t, GCPOptions{Instance: "main"})).Backup
	if b == nil || b.LastSuccessfulBackup == nil || !b.LastSuccessfulBackup.Equal(good) {
		t.Fatalf("backup = %+v, want last successful %s", b, good)
	}
	if b.FreshnessUnavailable != "" || b.NoRecentSuccess || !*b.PITREnabled {
		t.Fatalf("backup = %+v", b)
	}
	if len(f.backupQueries) != 1 || f.backupQueries[0].Get("maxResults") != "5" {
		t.Fatalf("backupRuns queries = %v, want one with maxResults=5", f.backupQueries)
	}
	if got, source, ok := b.LastGood(); !ok || !got.Equal(good) ||
		source != "last_successful_backup" {
		t.Fatalf("LastGood = %s, %q, %v", got, source, ok)
	}
}

// Boundary: only the 5 latest runs are read; failed runs and no success
// among them is evidence (backups fail), no runs at all is unknown.
func TestGCPCollectBackupRunsWithoutSuccess(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	var runs []map[string]any
	for i := 1; i <= 5; i++ {
		runs = append(runs, backupRun("FAILED", gcpNow.Add(-time.Duration(i)*24*time.Hour)))
	}
	f.backupRuns["main"] = append(runs, backupRun("SUCCESSFUL", gcpNow.Add(-6*24*time.Hour)))
	b := mustCollect(t, f.source(t, GCPOptions{Instance: "main"})).Backup
	if b.LastSuccessfulBackup != nil || !b.NoRecentSuccess {
		t.Fatalf("5 failed runs (the 6th successful one unread): %+v", b)
	}
	f2 := cloudSQLFixture(t, true, "0")
	b = mustCollect(t, f2.source(t, GCPOptions{Instance: "main"})).Backup
	if b.LastSuccessfulBackup != nil || b.NoRecentSuccess || b.FreshnessUnavailable != "" {
		t.Fatalf("no backup runs at all must be unknown, not failing: %+v", b)
	}
}

// Error propagation: a backupRuns 403 keeps the posture fields from the
// instance and the rest of the sample, and marks freshness unavailable
// with the provider's reason.
func TestGCPCollectBackupRunsDeniedKeepsThePosture(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	settings := f.instances["main"]["settings"].(map[string]any)
	settings["backupConfiguration"] = map[string]any{"enabled": true,
		"pointInTimeRecoveryEnabled": false, "transactionLogRetentionDays": 3}
	f.fail["backupruns"] = fakeFailure{status: 403, code: "PERMISSION_DENIED",
		message: "cloudsql.backupRuns.list denied"}
	s := mustCollect(t, f.source(t, GCPOptions{Instance: "main"}))
	b := s.Backup
	if b == nil || !*b.BackupsEnabled || *b.PITREnabled || *b.RetentionDays != 3 {
		t.Fatalf("posture fields lost on a 403: %+v", b)
	}
	if b.LastSuccessfulBackup != nil || !strings.Contains(b.FreshnessUnavailable, "403") ||
		!strings.Contains(b.FreshnessUnavailable, "backupRuns") {
		t.Fatalf("freshness = %q, want the 403 named", b.FreshnessUnavailable)
	}
	if s.CPUPct == nil {
		t.Fatal("the denied backup list dropped the rest of the sample")
	}
}

// The backup list is read at most every 15 minutes; a denial is cached
// for the same interval, so a 403 is not retried every poll.
func TestGCPBackupFreshnessIsCached(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	f.backupRuns["main"] = []map[string]any{backupRun("SUCCESSFUL", gcpNow.Add(-time.Hour))}
	src := f.source(t, GCPOptions{Instance: "main"})
	ctx := context.Background()
	for _, after := range []time.Duration{0, time.Minute, 14 * time.Minute} {
		src.backupFreshness(ctx, "main", gcpNow.Add(after))
	}
	if n := f.called("backupruns"); n != 1 {
		t.Fatalf("backupRuns calls within 15 minutes = %d, want 1", n)
	}
	got := src.backupFreshness(ctx, "main", gcpNow.Add(15*time.Minute))
	if n := f.called("backupruns"); n != 2 || got.last == nil {
		t.Fatalf("after 15 minutes: calls = %d, freshness = %+v", n, got)
	}
	f.fail["backupruns"] = fakeFailure{status: 403, code: "PERMISSION_DENIED", message: "no"}
	denied := src.backupFreshness(ctx, "main", gcpNow.Add(31*time.Minute))
	again := src.backupFreshness(ctx, "main", gcpNow.Add(32*time.Minute))
	if denied.reason == "" || again.reason != denied.reason || f.called("backupruns") != 3 {
		t.Fatalf("denied = %+v, again = %+v, calls = %d", denied, again,
			f.called("backupruns"))
	}
}

// A cancelled poll is not cached as unavailable freshness.
func TestGCPBackupFreshnessCancelledIsNotCached(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	f.backupRuns["main"] = []map[string]any{backupRun("SUCCESSFUL", gcpNow.Add(-time.Hour))}
	src := f.source(t, GCPOptions{Instance: "main"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := src.backupFreshness(ctx, "main", gcpNow)
	if got.last != nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("cancelled read = %+v", got)
	}
	got = src.backupFreshness(context.Background(), "main", gcpNow.Add(time.Second))
	if got.last == nil {
		t.Fatalf("after a cancelled read the next poll did not read again: %+v", got)
	}
}

func TestBackupPostureCloneCopiesFreshness(t *testing.T) {
	at := awsNow.Add(-time.Hour)
	b := &BackupPosture{LatestRestorableTime: &at, LastSuccessfulBackup: &at,
		FreshnessUnavailable: "x", NoRecentSuccess: true}
	c := b.clone()
	*c.LatestRestorableTime = awsNow
	*c.LastSuccessfulBackup = awsNow
	if !b.LatestRestorableTime.Equal(at) || !b.LastSuccessfulBackup.Equal(at) ||
		c.FreshnessUnavailable != "x" || !c.NoRecentSuccess {
		t.Fatalf("clone shares or drops freshness: %+v / %+v", b, c)
	}
}
