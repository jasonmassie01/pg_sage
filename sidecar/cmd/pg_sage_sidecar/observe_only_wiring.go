package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// observePromoteTimeout bounds the policy read that decides a promotion.
const observePromoteTimeout = 5 * time.Second

// startObserveSweeper starts the fleet-level sweeper of the observe-only
// members (fleetpol-01d) in a meta-db fleet. It runs whether or not any
// exist: a round with no members does no work.
func startObserveSweeper(state *metaDBState) {
	if state == nil || state.Store == nil || fleetMgr == nil {
		return
	}
	census := fleet.NewCensusStore(state.Pool)
	sweeper := fleet.NewObserveSweeper(state.Store, fleet.ObserveOptions{
		Members:  fleetMgr.SetObserveOnly,
		Activity: census.RecordActivity,
		Promote:  observePromotion(shutdownCtx, policy.NewStore(state.Pool)),
		Logf: func(level, format string, args ...any) {
			logStructured(level, "observe-only", format, args...)
		},
	})
	logInfo("observe-only", "observe-only sweeper: every %s, %d members at once",
		sweeper.Interval(), sweeper.Concurrency())
	go sweeper.Run(shutdownCtx)
	startSprawlCensus(census) // census_wiring.go (fleetpol-03)
}

// observeSetReader is what a promotion reads (a *policy.Store).
type observeSetReader interface {
	Resolve(ctx context.Context, databaseID int64) (policy.Resolution, error)
	Sets(ctx context.Context) ([]policy.SetDocument, error)
}

// observePromotion promotes a member whose policy set says
// enrollment.tier: full. A read failure never promotes (fail closed).
func observePromotion(base context.Context,
	ps observeSetReader) func(store.DatabaseRecord) bool {
	return func(rec store.DatabaseRecord) bool {
		ctx, cancel := context.WithTimeout(base, observePromoteTimeout)
		defer cancel()
		res, err := ps.Resolve(ctx, int64(rec.ID))
		if err != nil || res.Set == "" {
			return false
		}
		docs, err := ps.Sets(ctx)
		if err != nil {
			return false
		}
		for _, d := range docs {
			if d.Set.Name == res.Set {
				return d.Set.PromotesToFull()
			}
		}
		return false
	}
}
