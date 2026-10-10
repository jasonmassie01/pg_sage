package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// AP-11 needs to know where the database runs: the detected cloud
// environment reaches posture, so a managed server is not judged by
// archive_mode.
func TestPostureConfig_CarriesTheCloudEnvironment(t *testing.T) {
	c := config.DefaultConfig()
	c.CloudEnvironment = "rds"
	if got := postureConfig(c).Platform.Provider; got != "rds" {
		t.Fatalf("platform provider = %q, want rds", got)
	}
	if got := postureConfig(nil).Platform; got.Provider != "" || got.Backup != nil {
		t.Fatalf("nil config platform = %+v, want unknown", got)
	}
}

func TestPostureBackup_MapsTheTelemetry(t *testing.T) {
	if postureBackup(nil) != nil {
		t.Fatal("no telemetry must stay unknown")
	}
	days, on, off := 7, true, false
	got := postureBackup(&cloudtel.BackupPosture{BackupsEnabled: &on, PITREnabled: &off,
		RetentionDays: &days, DeletionProtection: &on})
	if got == nil || !*got.BackupsEnabled || *got.PITREnabled || *got.RetentionDays != 7 ||
		!*got.DeletionProtection {
		t.Fatalf("mapped backup posture = %+v", got)
	}
	unknown := postureBackup(&cloudtel.BackupPosture{})
	if unknown == nil || unknown.RetentionDays != nil || unknown.PITREnabled != nil {
		t.Fatalf("unknown fields must stay nil: %+v", unknown)
	}
}

// A runtime without cloud telemetry gives posture no backup posture.
func TestRuntimePostureBackup_NoTelemetry(t *testing.T) {
	rt := &databaseRuntime{}
	if rt.postureBackup() != nil {
		t.Fatal("a runtime without telemetry must report unknown backups")
	}
}
