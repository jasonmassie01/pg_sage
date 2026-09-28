package main

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rcaAdapter bridges *rca.Engine (which returns []rca.Incident)
// to the analyzer.RCAEngine interface (which returns nothing). It owns
// the engine's lifecycle context and durable store: every cycle first
// ensures the engine has loaded its open incidents from sage.incidents,
// and skips analysis when that state cannot be loaded (R04).
//
// The adapter is also the single registration point of Sage SRE for a
// database: buildDatabaseRuntime creates it for every mode (standalone,
// YAML fleet, meta-db, AgentDB). It starts the lock-chain fast path on the
// instance worker group and attaches the catalog probe runner used for
// narration evidence.
type rcaAdapter struct {
	e     *rca.Engine
	ctx   context.Context
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)

	fastPath *rca.LockChainTicker
	probes   *probes.Runner
}

var _ analyzer.RCAEngine = (*rcaAdapter)(nil)

// sreProbeLimiter bounds catalog probes across the whole sidecar.
var sreProbeLimiter = probes.NewLimiter(probes.MaxSidecarConcurrency)

// rcaAdapterDeps is one database's RCA wiring.
type rcaAdapterDeps struct {
	ctx     context.Context
	eng     *rca.Engine
	pool    *pgxpool.Pool
	name    string // the identity the executor and notifications use
	cfg     *config.Config
	logFn   func(string, string, ...any)
	workers *sync.WaitGroup // the instance worker group
}

// newRCAAdapter binds the engine to its database identity, store and
// lifecycle context, attaches the probe catalog and starts the fast path.
func newRCAAdapter(d rcaAdapterDeps) *rcaAdapter {
	d.eng.WithDatabaseName(d.name)
	a := &rcaAdapter{e: d.eng, ctx: d.ctx, pool: d.pool, logFn: d.logFn}
	if d.pool != nil {
		a.probes = probes.NewRunner(d.pool, probes.Catalog(), sreProbeLimiter)
		d.eng.WithProbes(a.probes)
	}
	a.startFastPath(d.cfg, d.workers)
	return a
}

func (a *rcaAdapter) Analyze(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) {
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

// startFastPath runs the lock-chain fast path on the instance worker
// group until the adapter's lifecycle context ends. It starts at
// registration: the analyzer's first cycle usually runs before the
// collector's first snapshot and skips RCA, which delayed the fast path
// by a whole analyzer interval. rca.lock_chain_interval_seconds = 0
// disables it.
func (a *rcaAdapter) startFastPath(cfg *config.Config, workers *sync.WaitGroup) {
	interval := cfg.RCA.LockChainInterval()
	switch {
	case interval <= 0:
		a.logFn("INFO", "rca: lock-chain fast path disabled")
		return
	case a.pool == nil || workers == nil:
		a.logFn("WARN", "rca: lock-chain fast path not started: no store "+
			"or worker group")
		return
	}
	probe := func(ctx context.Context) ([]analyzer.Finding, error) {
		return analyzer.ProbeLockChains(ctx, a.pool, cfg)
	}
	a.fastPath = rca.NewLockChainTicker(a.e, a.pool, probe, interval, a.logFn)
	startInstanceWorker(workers, func() { a.fastPath.Run(a.ctx) })
	a.logFn("INFO", "rca: lock-chain fast path every %s", interval)
}
