package main

import (
	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// postureBackup is the provider backup posture cloud telemetry last read,
// for AP-11; nil (unknown) without telemetry.
func (rt *databaseRuntime) postureBackup() *agentposture.BackupPosture {
	if rt.cloud == nil {
		return nil
	}
	return postureBackup(rt.cloud.Backup())
}

// postureBackup maps cloud telemetry's backup posture; the pointers are
// already copies, and nil fields stay unknown.
func postureBackup(b *cloudtel.BackupPosture) *agentposture.BackupPosture {
	if b == nil {
		return nil
	}
	return &agentposture.BackupPosture{BackupsEnabled: b.BackupsEnabled,
		PITREnabled: b.PITREnabled, RetentionDays: b.RetentionDays,
		DeletionProtection: b.DeletionProtection}
}
