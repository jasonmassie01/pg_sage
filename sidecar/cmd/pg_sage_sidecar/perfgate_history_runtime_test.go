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
	preserveParityGlobals(t, perfConfig(t, dsn, timing))
	preserveHistoryGlobals(t)
	cfg.Mode, cfg.MetaDB, cfg.History.Store = "standalone", metaDSN, "meta"
	storePool := perfStorePool(t, metaDSN)
	historyMetaPool = func() *pgxpool.Pool { return storePool }
	oldID := runtimeHistoryID
	runtimeHistoryID = func(databaseRuntimeSpec) int { return perfHistoryDatabaseID }
	t.Cleanup(func() { runtimeHistoryID = oldID })
	logs := capturePerfLogs(t)
	session := perfAPISession(t, ctx, harness)
	monitored, err := connectMonitoredDB(dsn, cfg.Postgres.MaxConnections)
	if err != nil {
		t.Fatalf("connect monitored pool: %v", err)
	}
	pool = monitored
	cloudEnvironment = detectCloudEnvironment()
	cfg.CloudEnvironment = cloudEnvironment

	base := readPerfCounters(t, ctx, harness, logs, true)
	metaBase := readPerfCounters(t, ctx, metaHarness, logs, true)
	initStandalone()
	stopStore := startHistoryStoreCleanerFor(storePool)
	router := perfRouter(t)
	stopWorkload := startPerfWorkload(t, dsn, scale.HotTables())
	time.Sleep(timing.Warmup)
	warm := readPerfCounters(t, ctx, harness, logs, true)
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
	storeWarmup := perfPhase("warmup (history store)", false, timing.Warmup, 0, metaBase,
		metaWarm)
	storeSteady := perfPhase("steady (history store)", true, timing.Window,
		timing.Cycles(), metaWarm, metaEnd)
	explainPerfPhases(t, ctx, harness, &warmup, &steady)
	explainPerfPhases(t, ctx, metaHarness, &storeWarmup, &storeSteady)
	return []perfgate.Phase{warmup, steady, storeWarmup, storeSteady}
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
