package main

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE runways (M6): the runway monitor on the collector tick and the
// advisor that attaches custodian proposals to pre-incident
// investigations.

// newFreezeCustodian is the database's freeze custodian. A database
// runtime builds one (freezeCustodian) for its autonomy worker, runway
// advisor and analyzer, which share its sampler: a reading is reused for
// half the autonomy interval, so each worker tick takes a fresh one.
func newFreezeCustodian(pool *pgxpool.Pool, cfg *config.Config,
	database string) *autonomy.PostgresFreezeCustodian {
	return autonomy.NewPostgresFreezeCustodian(pool, database,
		cfg.Custodian.Freeze.RedBufferPct).
		WithBloatBounds(cfg.Analyzer.BloatPlanMinBytes(), catalogread.FromSafety(cfg.Safety)).
		WithSampleTTL(autonomyInterval(cfg) / 2)
}

// freezeCustodian is the runtime's one freeze custodian, built on first use.
func (rt *databaseRuntime) freezeCustodian() *autonomy.PostgresFreezeCustodian {
	rt.freezeOnce.Do(func() { rt.freeze = newFreezeCustodian(rt.spec.Pool, rt.cfg, rt.spec.Name) })
	return rt.freeze
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
// custodians (the runtime's freeze custodian); the executor is attached
// once it is built.
func newRunwayAdvisorFor(freeze *autonomy.PostgresFreezeCustodian, pool *pgxpool.Pool,
	cfg *config.Config, database string) *runwayAdvisor {
	return newRunwayAdvisor(freeze, newWALCustodian(pool, cfg, database), haReplicaProbe(pool))
}

func runwayOptions(cfg *config.Config, database string) runway.Options {
	r := cfg.SRE.Runways
	return runway.Options{Database: database, Investigate: r.Investigate,
		Interval: r.Interval(), SequenceInterval: r.SequenceInterval(),
		Lookback: r.Lookback(), MinSamples: r.MinSamples, SizeInterval: r.SizeInterval(),
		MinSpan: r.MinSpan(), Retention: r.SampleRetention(),
		WraparoundHorizon: r.WraparoundHorizon(), WraparoundCritical: r.WraparoundCritical(),
		DiskHorizon: r.DiskHorizon(), DiskCritical: r.DiskCritical(),
		SequenceHorizon: r.SequenceHorizon(), SequenceCritical: r.SequenceCritical(),
		DiskCapacityBytes:     float64(cfg.Forecaster.DiskCapacityBytes),
		WALRetainedLimitBytes: float64(autonomy.DefaultWALBackstopBytes),
		Sizes:                 runwaySizeShare}
}

// runwaySizeShare is shared by every database runtime of the process, so
// fleet runtimes on one cluster measure its databases' total size once
// per size interval between them.
var runwaySizeShare = runway.NewSizeShare()

// startRunways attaches the executor to the runway advisor and starts the
// runway monitor on the instance worker group. Pre-incident
// investigations start through the investigator when it runs; without it
// the monitor still opens forecast findings.
func (rt *databaseRuntime) startRunways() {
	if !rt.sreReadPolicy().Allows(probes.ClusterDatabaseSizeProbe) {
		logInfo(rt.spec.Scope, "db %q: cluster runway sampler disabled by read scope; direct database probes remain available", rt.spec.Name)
		return
	}
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
	rt.start(func() { m.Run(probes.WithCaller(rt.ctx, probes.CallerRunway)) })
	rt.note("runways")
}
