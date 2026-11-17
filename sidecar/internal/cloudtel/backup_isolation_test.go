package cloudtel

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// drill-01b: does a backup survive the deletion of the instance, and how
// wide is the point-in-time window? RDS reads manual snapshots and the
// automated backups' restore window (one bounded call each, cached 6 h);
// Aurora reads cluster snapshots and the cluster's restorable times;
// Cloud SQL reads settings.retainBackupsOnDelete. Unknown stays nil.

const isoMaxAge = 26 * time.Hour

func isolationFixture(t *testing.T) *fakeAWS {
	t.Helper()
	f := rdsFixture(t, true)
	f.instances["orders"] = withBackupXML(f.instances["orders"],
		"<BackupRetentionPeriod>7</BackupRetentionPeriod>"+
			restorableXML(awsNow.Add(-5*time.Minute)))
	return f
}

func isolationSource(t *testing.T, f *fakeAWS) *AWSSource {
	t.Helper()
	return f.source(t, AWSOptions{InstanceID: "orders", IsolationMaxAge: isoMaxAge})
}

func wantSurvives(t *testing.T, b *BackupPosture, want bool, evidence string) {
	t.Helper()
	if b == nil || b.SurvivesDelete == nil || *b.SurvivesDelete != want {
		t.Fatalf("backup = %+v, want SurvivesDelete %v", b, want)
	}
	if !strings.Contains(b.IsolationEvidence, evidence) || b.IsolationUnavailable != "" {
		t.Fatalf("evidence = %q (unavailable %q), want one containing %q",
			b.IsolationEvidence, b.IsolationUnavailable, evidence)
	}
}

func TestIsolationManualSnapshotSurvivesDelete(t *testing.T) {
	f := isolationFixture(t)
	f.snapshots["instance:orders"] = []fakeSnapshot{snapshotAt(awsNow.Add(-10 * time.Hour))}
	b := mustCollect(t, isolationSource(t, f)).Backup
	wantSurvives(t, b, true, "manual snapshot")
	if *b.RetentionDays != 7 || b.LatestRestorableTime == nil {
		t.Fatalf("the other posture fields were lost: %+v", b)
	}
	var snapQuery bool
	for _, q := range f.recordQueries {
		if q.Get("Action") == "DescribeDBSnapshots" {
			snapQuery = true
			if q.Get("SnapshotType") != "manual" || q.Get("MaxRecords") != "20" ||
				q.Get("DBInstanceIdentifier") != "orders" {
				t.Fatalf("DescribeDBSnapshots query = %v, want manual, 20, orders", q)
			}
		}
	}
	if !snapQuery {
		t.Fatalf("no DescribeDBSnapshots call: %v", f.recordQueries)
	}
}

func TestIsolationAutomatedOnlySharesTheDeletePath(t *testing.T) {
	f := isolationFixture(t)
	f.autoBackups["db-ORDERS"] = automatedBackupXML("active", awsNow.Add(-7*24*time.Hour),
		awsNow.Add(-5*time.Minute))
	b := mustCollect(t, isolationSource(t, f)).Backup
	wantSurvives(t, b, false, "only automated backups")
	if !strings.Contains(b.IsolationEvidence, "AWS Backup vaults are not read") {
		t.Fatalf("evidence %q does not say AWS Backup vaults are not read",
			b.IsolationEvidence)
	}
}

// Invalid input: a snapshot without SnapshotCreateTime, or one still being
// created, is no proof of a fresh copy.
func TestIsolationSnapshotWithoutCreateTimeIsIgnored(t *testing.T) {
	f := isolationFixture(t)
	creating := snapshotAt(awsNow.Add(-time.Hour))
	creating.status = "creating"
	f.snapshots["instance:orders"] = []fakeSnapshot{{status: "available"}, creating}
	wantSurvives(t, mustCollect(t, isolationSource(t, f)).Backup, false,
		"only automated backups")
}

// Nil/empty: no snapshots and no automated backups is still evidence
// (nothing outlives the instance); a posture never read is unknown.
func TestIsolationEmptyListsShareTheDeletePath(t *testing.T) {
	f := isolationFixture(t)
	b := mustCollect(t, isolationSource(t, f)).Backup
	wantSurvives(t, b, false, "only automated backups")
	if b.PITRWindow != nil {
		t.Fatalf("no automated backup listed, yet PITRWindow = %v", *b.PITRWindow)
	}
	var zero BackupPosture
	if zero.SurvivesDelete != nil || zero.PITRWindow != nil {
		t.Fatal("a zero posture claims isolation evidence")
	}
	var none *BackupPosture
	if none.clone() != nil {
		t.Fatal("nil posture cloned to non-nil")
	}
}

// Error propagation: a 403 keeps every other field, leaves isolation
// unknown and says why; Collect itself succeeds.
func TestIsolationAccessDeniedKeepsTheOtherFields(t *testing.T) {
	f := isolationFixture(t)
	f.fail["rds:DescribeDBSnapshots"] = fakeFailure{status: 403, code: "AccessDenied",
		message: "not authorized to perform rds:DescribeDBSnapshots"}
	b := mustCollect(t, isolationSource(t, f)).Backup
	if b.SurvivesDelete != nil {
		t.Fatalf("SurvivesDelete = %v after a 403, want unknown", *b.SurvivesDelete)
	}
	if !strings.Contains(b.IsolationUnavailable, "DescribeDBSnapshots") {
		t.Fatalf("IsolationUnavailable = %q, want the failed call named",
			b.IsolationUnavailable)
	}
	if b.RetentionDays == nil || *b.RetentionDays != 7 || b.LatestRestorableTime == nil {
		t.Fatalf("a 403 lost the other posture fields: %+v", b)
	}
}

// Boundary: a snapshot exactly max age old counts; one second older not.
func TestIsolationSnapshotAgeBoundary(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{isoMaxAge, true}, {isoMaxAge + time.Second, false}} {
		f := isolationFixture(t)
		f.snapshots["instance:orders"] = []fakeSnapshot{snapshotAt(awsNow.Add(-tc.age))}
		b := mustCollect(t, isolationSource(t, f)).Backup
		if b.SurvivesDelete == nil || *b.SurvivesDelete != tc.want {
			t.Fatalf("snapshot %v old: %+v, want SurvivesDelete %v", tc.age, b, tc.want)
		}
	}
}

// A zero IsolationMaxAge is the 26 h default, never "any snapshot ages out".
func TestIsolationDefaultMaxAge(t *testing.T) {
	f := isolationFixture(t)
	f.snapshots["instance:orders"] = []fakeSnapshot{snapshotAt(awsNow.Add(-25 * time.Hour))}
	b := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"})).Backup
	wantSurvives(t, b, true, "manual snapshot")
}

func TestIsolationRDSPITRWindowFromTheRestoreWindow(t *testing.T) {
	f := isolationFixture(t)
	latest := awsNow.Add(-5 * time.Minute)
	f.autoBackups["db-ORDERS"] = automatedBackupXML("retained", latest.Add(-90*time.Hour),
		latest) + automatedBackupXML("active", latest.Add(-24*time.Hour), latest)
	b := mustCollect(t, isolationSource(t, f)).Backup
	if b.PITRWindow == nil || *b.PITRWindow != 24*time.Hour {
		t.Fatalf("PITRWindow = %v, want the active backup's 24h", b.PITRWindow)
	}
	// Invalid input: a window missing an end is no window.
	f2 := isolationFixture(t)
	f2.autoBackups["db-ORDERS"] = automatedBackupXML("active", time.Time{}, latest)
	if b := mustCollect(t, isolationSource(t, f2)).Backup; b.PITRWindow != nil {
		t.Fatalf("half a restore window gave PITRWindow %v", *b.PITRWindow)
	}
}

// Aurora: cluster snapshots, and the window from the cluster's own
// restorable times; no instance automated-backup call.
func TestIsolationAuroraReadsTheCluster(t *testing.T) {
	latest := awsNow.Add(-5 * time.Minute)
	f := newFakeAWS(t, awsNow)
	f.instances["shop-1"] = withBackupXML(instanceXML("shop-1", "db.r7g.xlarge",
		"aurora-postgresql", "shop-pg16", "in-sync", 1, 0, false, "shop"),
		"<BackupRetentionPeriod>1</BackupRetentionPeriod>")
	f.clusters["shop"] = strings.Replace(clusterXML("shop", "shop-1"), "</DBCluster>",
		restorableXML(latest)+"<EarliestRestorableTime>"+
			latest.Add(-(23*time.Hour+59*time.Minute)).UTC().Format(time.RFC3339)+
			"</EarliestRestorableTime></DBCluster>", 1)
	f.metric("shop-1", "CPUUtilization", time.Minute, 12)
	f.metric("shop-1", "FreeableMemory", time.Minute, 20*gib)
	f.snapshots["cluster:shop"] = []fakeSnapshot{snapshotAt(awsNow.Add(-2 * time.Hour))}
	b := mustCollect(t, f.source(t, AWSOptions{ClusterID: "shop"})).Backup
	wantSurvives(t, b, true, "manual cluster snapshot")
	if b.PITRWindow == nil || *b.PITRWindow != 23*time.Hour+59*time.Minute {
		t.Fatalf("Aurora PITRWindow = %v, want 23h59m", b.PITRWindow)
	}
	if n := f.called("rds:DescribeDBInstanceAutomatedBackups"); n != 0 {
		t.Fatalf("Aurora read instance automated backups %d times", n)
	}
	if n := f.called("rds:DescribeDBSnapshots"); n != 0 {
		t.Fatalf("Aurora read instance snapshots %d times", n)
	}
}

// AWS Backup and cross-region replication are in the instance
// description: either survives a delete with no manual snapshot.
func TestIsolationAWSBackupAndReplicationSurvive(t *testing.T) {
	for _, tc := range []struct{ xml, evidence string }{
		{"<AwsBackupRecoveryPointArn>arn:aws:backup:us-east-1:1:recovery-point:x" +
			"</AwsBackupRecoveryPointArn>", "AWS Backup"},
		{"<DBInstanceAutomatedBackupsReplications><DBInstanceAutomatedBackupsReplication>" +
			"<DBInstanceAutomatedBackupsArn>arn:aws:rds:us-west-2:1:auto-backup:ab-1" +
			"</DBInstanceAutomatedBackupsArn></DBInstanceAutomatedBackupsReplication>" +
			"</DBInstanceAutomatedBackupsReplications>", "replicated"},
	} {
		f := isolationFixture(t)
		f.instances["orders"] = withBackupXML(f.instances["orders"], tc.xml)
		wantSurvives(t, mustCollect(t, isolationSource(t, f)).Backup, true, tc.evidence)
	}
}

// The reads are cached 6 h (a failed read 15 min); the snapshot's age is
// judged at every collection, so a cached snapshot ages out.
func TestIsolationReadsAreCached(t *testing.T) {
	f := isolationFixture(t)
	f.snapshots["instance:orders"] = []fakeSnapshot{snapshotAt(awsNow.Add(-20 * time.Hour))}
	src := isolationSource(t, f)
	collect := func(at time.Time) *BackupPosture {
		t.Helper()
		s, err := src.Collect(context.Background(), at)
		if err != nil {
			t.Fatalf("Collect at %s: %v", at, err)
		}
		return s.Backup
	}
	wantSurvives(t, collect(awsNow), true, "manual snapshot")
	b := collect(awsNow.Add(5*time.Hour + 59*time.Minute))
	if n := f.called("rds:DescribeDBSnapshots"); n != 1 {
		t.Fatalf("DescribeDBSnapshots called %d times within 6 h, want 1", n)
	}
	if b.SurvivesDelete == nil || !*b.SurvivesDelete {
		t.Fatalf("a cached snapshot 25h59m old must still count: %+v", b)
	}
	b = collect(awsNow.Add(6*time.Hour + time.Second))
	if n := f.called("rds:DescribeDBSnapshots"); n != 2 {
		t.Fatalf("DescribeDBSnapshots called %d times after 6 h, want 2", n)
	}
	if b.SurvivesDelete == nil || *b.SurvivesDelete {
		t.Fatalf("a snapshot 26h1s old must not count: %+v", b)
	}
}

func TestIsolationFailedReadIsRetriedSooner(t *testing.T) {
	f := isolationFixture(t)
	f.fail["rds:DescribeDBSnapshots"] = fakeFailure{status: 403, code: "AccessDenied",
		message: "denied"}
	src := isolationSource(t, f)
	for _, at := range []time.Time{awsNow, awsNow.Add(14 * time.Minute)} {
		if _, err := src.Collect(context.Background(), at); err != nil {
			t.Fatalf("Collect: %v", err)
		}
	}
	if n := f.called("rds:DescribeDBSnapshots"); n != 1 {
		t.Fatalf("a failed read was retried %d times within 15 min, want 1 call", n)
	}
	if _, err := src.Collect(context.Background(), awsNow.Add(15*time.Minute)); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if n := f.called("rds:DescribeDBSnapshots"); n != 2 {
		t.Fatalf("DescribeDBSnapshots called %d times after 15 min, want 2", n)
	}
}

// Concurrency: Backup() readers during polls see whole postures, and two
// concurrent collections share the source's cache safely (run with
// -race in the golang:1.25 container).
func TestIsolationConcurrentBackupReadsDuringPoll(t *testing.T) {
	f := isolationFixture(t)
	f.snapshots["instance:orders"] = []fakeSnapshot{snapshotAt(awsNow.Add(-time.Hour))}
	rt, err := NewRuntime(isolationSource(t, f), RuntimeOptions{Database: "orders",
		Now: func() time.Time { return awsNow }})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if b := rt.Backup(); b != nil && b.SurvivesDelete != nil && !*b.SurvivesDelete {
					t.Errorf("reader saw SurvivesDelete false for a 1 h old snapshot")
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.Poll(context.Background()); err != nil {
				t.Errorf("Poll: %v", err)
			}
		}()
	}
	wg.Wait()
	b := rt.Backup()
	if b == nil || b.SurvivesDelete == nil || !*b.SurvivesDelete {
		t.Fatalf("after the polls: %+v", b)
	}
	*b.SurvivesDelete = false
	if again := rt.Backup(); !*again.SurvivesDelete {
		t.Fatal("Backup() handed out the runtime's own SurvivesDelete pointer")
	}
}

func TestIsolationCloneCopiesThePointers(t *testing.T) {
	yes, window := true, 24*time.Hour
	b := &BackupPosture{SurvivesDelete: &yes, PITRWindow: &window, IsolationEvidence: "e"}
	c := b.clone()
	*c.SurvivesDelete, *c.PITRWindow = false, time.Hour
	if !*b.SurvivesDelete || *b.PITRWindow != 24*time.Hour || c.IsolationEvidence != "e" {
		t.Fatalf("clone shares pointers: %+v / %+v", b, c)
	}
}

// Cloud SQL: settings.retainBackupsOnDelete decides; absent is unknown.
// The window is the transaction log retention when PITR is on.
func TestIsolationCloudSQLRetainOnDelete(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		retain   any
		want     *bool
		evidence string
	}{{true, &yes, "retains"}, {false, &no, "deletes"}, {nil, nil, ""}} {
		f := cloudSQLFixture(t, true, "0")
		settings := f.instances["main"]["settings"].(map[string]any)
		settings["backupConfiguration"] = map[string]any{"enabled": true,
			"pointInTimeRecoveryEnabled": true, "transactionLogRetentionDays": 7}
		if tc.retain != nil {
			settings["retainBackupsOnDelete"] = tc.retain
		}
		b := mustCollect(t, f.source(t, GCPOptions{Instance: "main"})).Backup
		if tc.want == nil {
			if b.SurvivesDelete != nil || b.IsolationEvidence != "" {
				t.Fatalf("absent retainBackupsOnDelete: %+v, want unknown", b)
			}
		} else {
			wantSurvives(t, b, *tc.want, tc.evidence)
		}
		if b.PITRWindow == nil || *b.PITRWindow != 7*24*time.Hour {
			t.Fatalf("Cloud SQL PITRWindow = %v, want 168h", b.PITRWindow)
		}
	}
}

// Cloud SQL with PITR off has no window (AP-11 reports PITR off).
func TestIsolationCloudSQLWithoutPITRHasNoWindow(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	settings := f.instances["main"]["settings"].(map[string]any)
	settings["backupConfiguration"] = map[string]any{"enabled": true,
		"pointInTimeRecoveryEnabled": false, "transactionLogRetentionDays": 7}
	b := mustCollect(t, f.source(t, GCPOptions{Instance: "main"})).Backup
	if b.PITRWindow != nil {
		t.Fatalf("PITR off, yet PITRWindow = %v", *b.PITRWindow)
	}
}
