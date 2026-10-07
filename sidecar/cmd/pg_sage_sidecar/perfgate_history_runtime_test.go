//go:build perfgate

package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// runPerfRuntimeWithStore is runPerfRuntime with the runtime's history in
// the store: the warmup and steady phases are read on both databases.
func runPerfRuntimeWithStore(
	t *testing.T, ctx context.Context, harness, metaHarness *pgxpool.Pool,
	dsn, metaDSN string, scale perfgate.Scale, timing perfgate.Timing,
) []perfgate.Phase {
	t.Helper()
	storePool := perfHistoryGlobals(t, dsn, metaDSN, timing)
	logs := capturePerfLogs(t)
	session := perfAPISession(t, ctx, harness)
	monitored, err := connectMonitoredDB(dsn, cfg.Postgres.MaxConnections)
	if err != nil {
		t.Fatalf("connect monitored pool: %v", err)
	}
	pool = monitored
	cloudEnvironment = detectCloudEnvironment()
	cfg.CloudEnvironment = cloudEnvironment

	// pg_stat_statements_reset is server-wide: read both databases, then reset.
	base := readPerfCounters(t, ctx, harness, logs, false)
	metaBase := readPerfCounters(t, ctx, metaHarness, logs, true)
	initStandalone()
	stopStore := startHistoryStoreCleanerFor(storePool)
	router := perfRouter(t)
	stopWorkload := startPerfWorkload(t, dsn, scale.HotTables())
	time.Sleep(timing.Warmup)
	warm := readPerfCounters(t, ctx, harness, logs, false)
	metaWarm := readPerfCounters(t, ctx, metaHarness, logs, true)

	steadyStart := time.Now()
	cpuStart := perfProcessCPU(t)
	time.Sleep(timing.Window / 2)
	apiStart := perfProcessCPU(t)
	endpoints := callPerfEndpoints(router, session, perfEndpointSamples(t))
	apiCPU := perfProcessCPU(t) - apiStart
	time.Sleep(time.Until(steadyStart.Add(timing.Window)))
	cpu := perfProcessCPU(t) - cpuStart - apiCPU
	stopWorkload()
	stopStore()
	stopPerfRuntime(t, monitored)
	storePool.Close()
	time.Sleep(1500 * time.Millisecond) // the store's backends flush their statistics
	end := readPerfCounters(t, ctx, harness, logs, false)
	metaEnd := readPerfCounters(t, ctx, metaHarness, logs, false)

	warmup := perfPhase("warmup", false, timing.Warmup, 0, base, warm)
	steady := perfPhase("steady", true, timing.Window, timing.Cycles(), warm, end)
	steady.Endpoints = endpoints
	steady.ProcessCPU, steady.CPUKnown = cpu, true
	explainPerfPhases(t, ctx, harness, &warmup, &steady)
	return append([]perfgate.Phase{warmup, steady},
		storePerfPhases(t, ctx, metaHarness, timing, metaBase, metaWarm, metaEnd)...)
}

// storePerfPhases are the history store's warmup and steady phases.
func storePerfPhases(t *testing.T, ctx context.Context, metaHarness *pgxpool.Pool,
	timing perfgate.Timing, base, warm, end perfCounters) []perfgate.Phase {
	t.Helper()
	warmup := perfPhase("warmup (history store)", false, timing.Warmup, 0, base, warm)
	steady := perfPhase("steady (history store)", true, timing.Window, timing.Cycles(),
		warm, end)
	explainPerfPhases(t, ctx, metaHarness, &warmup, &steady)
	return []perfgate.Phase{warmup, steady}
}

// perfHistoryGlobals sets the process up as runPerfRuntime does, with the
// history in the store: history.store meta, the store pool, and the
// runtime's history id (a standalone runtime has no meta-db record).
func perfHistoryGlobals(t *testing.T, dsn, metaDSN string, timing perfgate.Timing) (
	*pgxpool.Pool) {
	t.Helper()
	preserveParityGlobals(t, perfConfig(t, dsn, timing))
	preserveHistoryGlobals(t)
	cfg.Mode, cfg.MetaDB, cfg.History.Store = "standalone", metaDSN, "meta"
	storePool := perfStorePool(t, metaDSN)
	historyMetaPool = func() *pgxpool.Pool { return storePool }
	oldID := runtimeHistoryID
	runtimeHistoryID = func(databaseRuntimeSpec) int { return perfHistoryDatabaseID }
	t.Cleanup(func() { runtimeHistoryID = oldID })
	return storePool
}

// perfStorePool is the runtime's pool on the meta database, configured as
// production configures it (connectMetaDB).
func perfStorePool(t *testing.T, metaDSN string) *pgxpool.Pool {
	t.Helper()
	p, err := connectMetaDB(metaDSN)
	if err != nil {
		t.Fatalf("connect store: %v", err)
	}
	return p
}
