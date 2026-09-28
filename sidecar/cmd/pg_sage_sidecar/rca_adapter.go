package main

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// rcaAdapter bridges *rca.Engine (which returns []rca.Incident)
// to the analyzer.RCAEngine interface (which returns nothing). It owns
// the engine's lifecycle context and durable store: every cycle first
// ensures the engine has loaded its open incidents from sage.incidents,
// and skips analysis when that state cannot be loaded (R04).
//
// The adapter is also the single registration point of the Sage SRE
// lock-chain fast path: the first analyzer cycle starts it for this
// database in every runtime mode (standalone, fleet, meta-db).
type rcaAdapter struct {
	e     *rca.Engine
	ctx   context.Context
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)

	fastOnce sync.Once
	fastPath *rca.LockChainTicker
}

var _ analyzer.RCAEngine = (*rcaAdapter)(nil)

// newRCAAdapter binds eng to its database identity (the name used by the
// executor and notifications for this database), its store and the
// runtime lifecycle context.
func newRCAAdapter(
	ctx context.Context,
	eng *rca.Engine,
	pool *pgxpool.Pool,
	databaseName string,
	logFn func(string, string, ...any),
) *rcaAdapter {
	eng.WithDatabaseName(databaseName)
	return &rcaAdapter{e: eng, ctx: ctx, pool: pool, logFn: logFn}
}

func (a *rcaAdapter) Analyze(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) {
	a.fastOnce.Do(func() { a.startFastPath(cfg) })
	if err := a.e.Hydrate(a.ctx, a.pool); err != nil {
		a.logFn("WARN", "rca: skipping cycle, incident state not "+
			"loaded: %v", err)
		return
	}
	a.e.AnalyzeContext(a.ctx, current, previous, cfg, lockChainFindings)
}

func (a *rcaAdapter) PersistIncidents(
	ctx context.Context,
	pool *pgxpool.Pool,
) error {
	return a.e.PersistIncidents(ctx, pool)
}

// startFastPath runs the lock-chain fast path until the adapter's
// lifecycle context ends. rca.lock_chain_interval_seconds = 0 disables it.
func (a *rcaAdapter) startFastPath(cfg *config.Config) {
	interval := cfg.RCA.LockChainInterval()
	if interval <= 0 || a.pool == nil {
		a.logFn("INFO", "rca: lock-chain fast path disabled")
		return
	}
	probe := func(ctx context.Context) ([]analyzer.Finding, error) {
		return analyzer.ProbeLockChains(ctx, a.pool, cfg)
	}
	a.fastPath = rca.NewLockChainTicker(a.e, a.pool, probe, interval, a.logFn)
	go a.fastPath.Run(a.ctx)
	a.logFn("INFO", "rca: lock-chain fast path every %s", interval)
}
