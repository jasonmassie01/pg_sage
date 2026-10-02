package main

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/executor"
)

// drainRuntimeWorkers waits, within ctx, for every database runtime's
// workers (autonomy drain, provider observability, orchestrators) once
// shutdownCtx has been cancelled.
func drainRuntimeWorkers(ctx context.Context) {
	if fleetMgr == nil {
		return
	}
	for name, inst := range fleetMgr.Instances() {
		if inst.Workers != nil && !waitProviderWorkers(ctx, inst.Workers) {
			logWarn("shutdown", "db %q: runtime workers exceeded shutdown deadline", name)
		}
	}
}

// shutdownExecutors calls Shutdown on every registered executor
// (standalone + each fleet instance) in parallel, bounded by the
// supplied context.
func shutdownExecutors(ctx context.Context) {
	var execs []*executor.Executor
	if exec != nil {
		execs = append(execs, exec)
	}
	if fleetMgr != nil {
		for _, inst := range fleetMgr.Instances() {
			if inst.Executor != nil {
				execs = append(execs, inst.Executor)
			}
		}
	}
	if len(execs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, e := range execs {
		wg.Add(1)
		go func(ex *executor.Executor) {
			defer wg.Done()
			if err := ex.Shutdown(ctx); err != nil {
				logWarn("shutdown",
					"executor: %v", err)
			}
		}(e)
	}
	wg.Wait()
}

// shutdownProcess runs the graceful shutdown after sig: cancel background
// work, stop the HTTP servers, drain runtimes and executors, then exit with
// the restart code when a restart was requested.
func shutdownProcess(sig os.Signal, promServer *http.Server) {
	logInfo("shutdown", "received %s, shutting down…", sig)
	shutdownCancel()

	// Hard deadline: if graceful shutdown doesn't complete in 10s,
	// force exit so Ctrl+C never hangs indefinitely.
	go func() {
		time.Sleep(10 * time.Second)
		logError("shutdown", "timed out after 10s, forcing exit")
		os.Exit(forcedShutdownExitCode())
	}()

	if rateLimiterInstance != nil {
		rateLimiterInstance.Stop()
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(),
		8*time.Second)
	defer shutCancel()

	stopHTTPServers(shutCtx, promServer)

	// Drain executors so any rollback monitors started via
	// MonitorAndRollback finish cleanly (or time out) instead
	// of leaking. The per-executor Shutdown waits on its
	// internal WaitGroup, so we can run them in parallel.
	drainRuntimeWorkers(shutCtx)
	shutdownExecutors(shutCtx)

	logInfo("shutdown", "stopped")

	// If this was a restart request, exit with the restart code so the
	// supervisor (launcher loop / orchestrator) relaunches the process.
	if restartRequested.Load() {
		logInfo("shutdown", "restarting (exit %d)", restartExitCode)
		os.Exit(restartExitCode)
	}
}

// stopHTTPServers shuts down the Prometheus and API servers and the login
// rate-limiter's cleanup goroutine.
func stopHTTPServers(ctx context.Context, promServer *http.Server) {
	if err := promServer.Shutdown(ctx); err != nil {
		logWarn("shutdown", "Prometheus server: %v", err)
	}
	if apiServer != nil {
		if err := apiServer.Shutdown(ctx); err != nil {
			logWarn("shutdown", "API server: %v", err)
		}
	}
	// Stop the login rate-limiter's cleanup goroutine so it
	// doesn't outlive the API server.
	api.ShutdownLoginLimiter()
}
