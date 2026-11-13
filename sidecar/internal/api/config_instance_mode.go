package api

import "github.com/pg-sage/sidecar/internal/fleet"

// instanceModeAndTrust is the database's effective execution mode and trust
// level: the running executor's, else its per-database config read through
// the locked snapshot, else approval and the global trust level.
func instanceModeAndTrust(
	instance *fleet.DatabaseInstance, globalTrust string,
) (string, string) {
	cfg := instance.ConfigSnapshot()
	executionMode, trustLevel := cfg.ExecutionMode, cfg.TrustLevel
	if instance.Executor != nil {
		executionMode = instance.Executor.ExecutionMode()
		trustLevel = instance.Executor.TrustLevel()
	}
	if executionMode == "" {
		executionMode = "approval"
	}
	if trustLevel == "" {
		trustLevel = globalTrust
	}
	return executionMode, trustLevel
}
