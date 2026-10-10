package cloudtel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// withBackupXML adds backup fields to a recorded DBInstance element.
func withBackupXML(inst, fields string) string {
	return strings.Replace(inst, "</DBInstance>", fields+"</DBInstance>", 1)
}

func TestAWSCollectRecordsBackupPosture(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = withBackupXML(f.instances["orders"],
		"<BackupRetentionPeriod>0</BackupRetentionPeriod>"+
			"<DeletionProtection>false</DeletionProtection>")
	s := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"}))
	b := s.Backup
	if b == nil || b.RetentionDays == nil || *b.RetentionDays != 0 ||
		b.DeletionProtection == nil || *b.DeletionProtection {
		t.Fatalf("backup posture = %+v, want retention 0 and no deletion protection", b)
	}
	if b.BackupsEnabled == nil || *b.BackupsEnabled || b.PITREnabled == nil || *b.PITREnabled {
		t.Fatalf("RDS retention 0 means no automated backups and no PITR: %+v", b)
	}
}

func TestAWSCollectBackupRetentionSevenDays(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = withBackupXML(f.instances["orders"],
		"<BackupRetentionPeriod>7</BackupRetentionPeriod>"+
			"<DeletionProtection>true</DeletionProtection>")
	b := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"})).Backup
	if b == nil || *b.RetentionDays != 7 || !*b.DeletionProtection || !*b.PITREnabled ||
		!*b.BackupsEnabled {
		t.Fatalf("backup posture = %+v", b)
	}
}

// A recorded response without the backup fields leaves them unknown (nil),
// never zero: posture must not report "retention 0" it did not see.
func TestAWSCollectBackupFieldsAbsentAreUnknown(t *testing.T) {
	f := rdsFixture(t, true)
	b := mustCollect(t, f.source(t, AWSOptions{InstanceID: "orders"})).Backup
	if b == nil {
		t.Fatal("backup posture is nil; want a posture with unknown fields")
	}
	if b.RetentionDays != nil || b.DeletionProtection != nil || b.PITREnabled != nil ||
		b.BackupsEnabled != nil {
		t.Fatalf("absent fields must stay unknown: %+v", b)
	}
}

// Aurora keeps deletion protection on the cluster, so the instance's flag
// is not evidence: it stays unknown.
func TestAWSCollectAuroraDeletionProtectionUnknown(t *testing.T) {
	f := newFakeAWS(t, awsNow)
	f.instances["shop-1"] = withBackupXML(instanceXML("shop-1", "db.r7g.xlarge",
		"aurora-postgresql", "shop-pg16", "in-sync", 1, 0, false, "shop"),
		"<BackupRetentionPeriod>1</BackupRetentionPeriod>"+
			"<DeletionProtection>false</DeletionProtection>")
	f.clusters["shop"] = clusterXML("shop", "shop-1")
	f.metric("shop-1", "CPUUtilization", time.Minute, 12)
	f.metric("shop-1", "FreeableMemory", time.Minute, 20*gib)
	s := mustCollect(t, f.source(t, AWSOptions{ClusterID: "shop"}))
	if s.Backup == nil || s.Backup.DeletionProtection != nil || *s.Backup.RetentionDays != 1 {
		t.Fatalf("aurora backup posture = %+v", s.Backup)
	}
}

func TestGCPCollectRecordsBackupPosture(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	settings := f.instances["main"]["settings"].(map[string]any)
	settings["backupConfiguration"] = map[string]any{"enabled": true,
		"pointInTimeRecoveryEnabled": false, "transactionLogRetentionDays": 7}
	settings["deletionProtectionEnabled"] = true
	s := mustCollect(t, f.source(t, GCPOptions{Instance: "main"}))
	b := s.Backup
	if b == nil || !*b.BackupsEnabled || *b.PITREnabled || *b.RetentionDays != 7 ||
		!*b.DeletionProtection {
		t.Fatalf("cloud sql backup posture = %+v", b)
	}
}

func TestGCPCollectBackupFieldsAbsentAreUnknown(t *testing.T) {
	f := cloudSQLFixture(t, true, "0")
	b := mustCollect(t, f.source(t, GCPOptions{Instance: "main"})).Backup
	if b == nil || b.BackupsEnabled != nil || b.PITREnabled != nil || b.RetentionDays != nil ||
		b.DeletionProtection != nil {
		t.Fatalf("absent Cloud SQL backup fields must stay unknown: %+v", b)
	}
}

// The runtime serves a copy of the latest sample's backup posture, and
// none once a poll fails (fail closed: unknown, not "fine").
func TestRuntimeBackupPosture(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = withBackupXML(f.instances["orders"],
		"<BackupRetentionPeriod>3</BackupRetentionPeriod>")
	rt, err := NewRuntime(f.source(t, AWSOptions{InstanceID: "orders"}),
		RuntimeOptions{Database: "app", Now: func() time.Time { return awsNow }})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if rt.Backup() != nil {
		t.Fatal("backup posture before any poll must be unknown")
	}
	if err := rt.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	b := rt.Backup()
	if b == nil || *b.RetentionDays != 3 {
		t.Fatalf("backup = %+v", b)
	}
	*b.RetentionDays = 99
	if *rt.Backup().RetentionDays != 3 {
		t.Fatal("Backup must return a copy")
	}
	f.fail["rds:DescribeDBInstances"] = fakeFailure{status: 500}
	_ = rt.Poll(context.Background())
	if rt.Backup() != nil {
		t.Fatal("a failed poll must leave the backup posture unknown")
	}
	var nilRT *Runtime
	if nilRT.Backup() != nil {
		t.Fatal("nil runtime has no backup posture")
	}
}
