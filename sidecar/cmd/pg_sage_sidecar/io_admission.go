package main

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/verify"
)

// ioSampleInterval is the pg-side IO sampling cadence (D6).
const ioSampleInterval = time.Minute

// databaseExecConfig clones the base config for one fleet database and
// applies that database's own load-admission attestation.
func databaseExecConfig(base *config.Config, db config.DatabaseConfig) *config.Config {
	execCfg := config.Clone(base)
	execCfg.Verify.IOCapacity = nil
	if capacity := db.Verify.IOCapacity; capacity != nil {
		declared := *capacity
		execCfg.Verify.IOCapacity = &declared
	}
	return execCfg
}

// startIOAdmission installs and runs the pg-side IO sampler that lets
// autonomous index builds earn load admission.
func startIOAdmission(
	ctx context.Context, workers *sync.WaitGroup, pool *pgxpool.Pool,
	cfg *config.Config, database string, exec *executor.Executor,
) *verify.IOMonitor {
	if pool == nil || cfg == nil || exec == nil {
		return nil
	}
	retention := time.Duration(cfg.Verify.IOSampleDays) * 24 * time.Hour
	monitor := verify.NewIOMonitor(pool, database, retention)
	exec.WithIOEvidence(monitor)
	startInstanceWorker(workers, func() {
		monitor.Run(ctx, ioSampleInterval, func(err error) {
			logWarn("admission", "db %q IO sample: %v", database, err)
		})
	})
	return monitor
}
