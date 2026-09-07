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
// to the analyzer.RCAEngine interface (which returns nothing).
type rcaAdapter struct {
	e *rca.Engine
}

var _ analyzer.RCAEngine = (*rcaAdapter)(nil)

func (a *rcaAdapter) Analyze(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) {
	a.e.Analyze(current, previous, cfg, lockChainFindings)
}

func (a *rcaAdapter) PersistIncidents(
	ctx context.Context,
	pool *pgxpool.Pool,
) error {
	return a.e.PersistIncidents(ctx, pool)
}
