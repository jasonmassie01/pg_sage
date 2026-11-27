package analyzer

import (
	"context"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// FreezeHorizonSource is the database's freeze sampler, shared with the
// freeze custodian and the runway advisor (autonomy.FreezeSampler).
type FreezeHorizonSource interface {
	FreezeHorizons(ctx context.Context) ([]probes.FreezeHorizon, error)
}

// WithFreezeHorizons attaches the database's freeze sampler.
func (a *Analyzer) WithFreezeHorizons(src FreezeHorizonSource) { a.freezeHorizons = src }

// loadCycleCatalogState reads the per-cycle catalog state the rules use:
// the statistics epoch, index builds in progress and the freeze horizons.
func (a *Analyzer) loadCycleCatalogState(ctx context.Context) {
	a.loadStatsEpoch(ctx)
	a.loadIndexBuilds(ctx)
	a.loadFreezeHorizons(ctx)
	a.loadArchiverOwner()
}

// loadFreezeHorizons refreshes extras.FreezeHorizons once per cycle. It
// fails closed: without a reading, wraparound_freeze is not resolved.
func (a *Analyzer) loadFreezeHorizons(ctx context.Context) {
	a.extras.FreezeHorizons = nil
	if a.freezeHorizons == nil {
		a.evalFail("wraparound_freeze")
		return
	}
	horizons, err := a.freezeHorizons.FreezeHorizons(ctx)
	if err != nil {
		a.logFn("WARN", "analyzer: freeze horizons: %v", err)
		a.evalFail("wraparound_freeze")
		return
	}
	a.extras.FreezeHorizons = horizons
}
