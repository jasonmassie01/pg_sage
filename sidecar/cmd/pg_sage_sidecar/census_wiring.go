package main

import (
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// startSprawlCensus starts the monthly sprawl census (fleetpol-03) of a
// meta-db fleet: the leader runs it once a month (the unique period makes
// two leaders' runs one), and the inbox lists its TTL proposals as advice.
// It only reads the monitored databases.
func startSprawlCensus(census *fleet.CensusDB) {
	if census == nil || fleetMgr == nil {
		return
	}
	fleetMgr.SetCensus(census)
	var rates cost.RateSource
	if cfg != nil && len(cfg.Cost.Rates) > 0 {
		rates = cost.NewConfigRates(cfg.Cost)
	}
	job := fleet.NewCensus(census, fleet.CensusOptions{
		Holder:   clone.ProcessHolder(),
		Rates:    rates,
		Measure:  fleet.NewCensusMeasure(fleetMgr.GetInstance, census, time.Now),
		IsLeader: func() bool { return currentFleetLeader().IsLeader() },
		Logf: func(level, format string, args ...any) {
			logStructured(level, "census", format, args...)
		},
	})
	logInfo("census", "sprawl census: one run a month on the fleet leader, checked every %s",
		fleet.DefaultCensusEvery)
	go job.Run(shutdownCtx)
}
