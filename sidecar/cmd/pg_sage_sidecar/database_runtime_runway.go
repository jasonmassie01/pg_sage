package main

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/runway"
)

// Sage SRE runways (M6): the runway monitor on the collector tick and the
// advisor that attaches custodian proposals to pre-incident
// investigations.

// newFreezeCustodian is the database's freeze custodian, as the
// continuous autonomy workers and the runway advisor build it.
func newFreezeCustodian(pool *pgxpool.Pool, cfg *config.Config,
	database string) *autonomy.PostgresFreezeCustodian {
	return autonomy.NewPostgresFreezeCustodian(pool, database,
		cfg.Custodian.Freeze.RedBufferPct)
}

// newWALCustodian is the database's WAL custodian. Drop stays disabled
// until policy supplies opt-in and an owner allowlist.
func newWALCustodian(pool *pgxpool.Pool, cfg *config.Config,
	database string) *autonomy.PostgresWALCustodian {
	return autonomy.NewPostgresWALCustodian(pool, database, autonomy.PostgresWALOptions{
		AbandonAfter: time.Duration(
			cfg.Custodian.WAL.AbandonAfterMinutes) * time.Minute,
		DiskPctCeiling: cfg.Custodian.WAL.RetainedWALDiskPctCeiling,
		AllowDrop:      false,
	})
}

// newRunwayAdvisorFor builds the advisor over the database's own
// custodians; the executor is attached once it is built.
func newRunwayAdvisorFor(pool *pgxpool.Pool, cfg *config.Config,
	database string) *runwayAdvisor {
	return newRunwayAdvisor(newFreezeCustodian(pool, cfg, database),
		newWALCustodian(pool, cfg, database), haReplicaProbe(pool))
}

func runwayOptions(cfg *config.Config, database string) runway.Options {
	r := cfg.SRE.Runways
	return runway.Options{Database: database, Investigate: r.Investigate,
		Interval: r.Interval(), Lookback: r.Lookback(), MinSamples: r.MinSamples,
		MinSpan: r.MinSpan(), Retention: r.SampleRetention(),
		WraparoundHorizon: r.WraparoundHorizon(), WraparoundCritical: r.WraparoundCritical(),
		DiskHorizon: r.DiskHorizon(), DiskCritical: r.DiskCritical(),
		SequenceHorizon: r.SequenceHorizon(), SequenceCritical: r.SequenceCritical(),
		DiskCapacityBytes:     float64(cfg.Forecaster.DiskCapacityBytes),
		WALRetainedLimitBytes: float64(autonomy.DefaultWALBackstopBytes)}
}

// startRunways attaches the executor to the runway advisor and starts the
// runway monitor on the instance worker group. Pre-incident
// investigations start through the investigator when it runs; without it
// the monitor still opens forecast findings.
func (rt *databaseRuntime) startRunways() {
	if rt.runwayAdvisor != nil && rt.executor != nil {
		rt.runwayAdvisor.attach(rt.executor)
	}
	if !rt.cfg.SRE.Runways.Enabled {
		return
	}
	var starter runway.Starter
	if rt.sre != nil {
		starter = rt.sre
	}
	m, err := runway.NewMonitor(rt.spec.Pool, rt.probes, starter,
		runwayOptions(rt.cfg, rt.spec.Name), logStructuredWrapper)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: runway monitor not started: %v", rt.spec.Name, err)
		return
	}
	rt.start(func() { m.Run(rt.ctx) })
	rt.note("runways")
}
