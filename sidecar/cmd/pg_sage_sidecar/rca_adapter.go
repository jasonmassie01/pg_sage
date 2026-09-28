package main

import (
	"context"

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
type rcaAdapter struct {
	e     *rca.Engine
	ctx   context.Context
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)
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
