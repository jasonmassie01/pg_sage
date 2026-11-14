package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetReconnectInterval is how often the meta-DB fleet reconciles and
// retries failed databases. Tests shrink it.
var fleetReconnectInterval = 30 * time.Second

// fleetReconnectLoop runs every fleetReconnectInterval (30s by default):
// it reconciles the fleet with the store, then retries every failed
// instance once. A retry's connect backs off from startupBackoff.
func fleetReconnectLoop(ctx context.Context, state *metaDBState) {
	ticker := time.NewTicker(fleetReconnectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Rows changed outside this process's API (another replica,
			// SQL) converge first; failed databases then retry.
			runMetaReconcilePass(ctx, state)
			retryFailedInstances(state)
		case <-ctx.Done():
			return
		}
	}
}

// retryFailedInstances finds instances with errors or nil pools
// and attempts reconnection. A store that cannot be read ends the tick.
func retryFailedInstances(state *metaDBState) {
	for name, inst := range fleetMgr.Instances() {
		if !needsReconnect(inst) {
			continue
		}
		if err := retryOneInstance(state, name, inst); err != nil {
			logWarn("reconnect", "load databases: %v", err)
			return
		}
	}
}

// needsReconnect reports whether inst is failed and not manually stopped.
func needsReconnect(inst *fleet.DatabaseInstance) bool {
	snap := inst.SnapshotStatus()
	if inst.Pool != nil && snap.Error == "" {
		return false // healthy
	}
	return !fleetMgr.InstanceStopped(inst) // a stopped instance stays down
}

// retryOneInstance reconnects the failed instance inst registered as name
// from its store record. It returns an error only when the store cannot be
// read; a database that is still unreachable is logged and stays failed.
func retryOneInstance(
	state *metaDBState, name string, inst *fleet.DatabaseInstance,
) error {
	logInfo("reconnect", "attempting reconnect for %q", name)

	ctx, cancel := context.WithTimeout(context.Background(), metaDBTimeout)
	defer cancel()
	records, err := loadDatabasesFromStore(ctx, state.Store)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Name == name {
			reconnectStoreRecord(ctx, state, name, inst, rec)
			break
		}
	}
	return nil
}

// reconnectStoreRecord builds a fresh runtime from rec and publishes it in
// place of inst when inst is still the registered generation.
func reconnectStoreRecord(
	ctx context.Context, state *metaDBState, name string,
	inst *fleet.DatabaseInstance, rec store.DatabaseRecord,
) {
	candidate, err := prepareStoreDatabase(ctx, state, rec)
	if err != nil {
		logWarn("reconnect", "db %q: still unreachable: %v", name, err)
		return
	}
	// Success — bootstrap and re-register.
	err = fleetMgr.ReplaceInstanceIfCurrent(
		ctx, name, inst, candidate, healthCheckStoreDatabase,
	)
	if err != nil {
		if fleetMgr.GetInstance(candidate.Name) == candidate {
			logWarn("reconnect",
				"db %q: published but old runtime drain failed: %v", name, err)
		} else {
			logWarn("reconnect", "db %q: candidate rejected: %v", name, err)
		}
		return
	}
	updateInstanceFindings(context.Background(), candidate)
	logInfo("reconnect", "db %q: reconnected successfully", name)
}

// storeRecordToDBConfig converts a store.DatabaseRecord to a
// config.DatabaseConfig for the fleet manager.
func storeRecordToDBConfig(
	rec store.DatabaseRecord,
) config.DatabaseConfig {
	return config.DatabaseConfig{
		Name:               rec.Name,
		Host:               rec.Host,
		Port:               rec.Port,
		Database:           rec.DatabaseName,
		User:               rec.Username,
		SSLMode:            rec.SSLMode,
		MaxConnections:     rec.MaxConnections,
		Tags:               recordTags(rec.Tags),
		TrustLevel:         rec.TrustLevel,
		TrustLevelExplicit: true,
		ExecutionMode:      resolveExecMode(rec),
	}
}

// resolveExecMode returns the execution mode for a store record.
func resolveExecMode(rec store.DatabaseRecord) string {
	if rec.ExecutionMode != "" {
		return rec.ExecutionMode
	}
	return "auto"
}
