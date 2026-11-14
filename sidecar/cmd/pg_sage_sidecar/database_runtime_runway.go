package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/ha"
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
		WithSampleTTL(autonomyInterval(cfg) / 2).
		WithLogger(logStructuredWrapper)
}

// freezeCustodian is the runtime's one freeze custodian, built on first use.
func (rt *databaseRuntime) freezeCustodian() *autonomy.PostgresFreezeCustodian {
	rt.freezeOnce.Do(func() { rt.freeze = newFreezeCustodian(rt.spec.Pool, rt.cfg, rt.spec.Name) })
	return rt.freeze
}

// newWALCustodian is the database's WAL custodian. Drop stays disabled
// until policy supplies opt-in and an owner allowlist. disk (nil without
// managed-cloud telemetry) is the denominator of the retained-WAL disk
// percentage ceiling.
func newWALCustodian(pool *pgxpool.Pool, cfg *config.Config, database string,
	disk autonomy.DiskCapacityProvider) *autonomy.PostgresWALCustodian {
	return autonomy.NewPostgresWALCustodian(pool, database, autonomy.PostgresWALOptions{
		AbandonAfter: time.Duration(
			cfg.Custodian.WAL.AbandonAfterMinutes) * time.Minute,
		DiskPctCeiling: cfg.Custodian.WAL.RetainedWALDiskPctCeiling,
		AllowDrop:      false,
		Disk:           disk,
	})
}

// newRunwayAdvisorFor builds the advisor over the database's own
// custodians (the runtime's freeze custodian), its provider storage
// capacity and its HA monitor's snapshot; the executor is attached once it
// is built.
func newRunwayAdvisorFor(freeze *autonomy.PostgresFreezeCustodian, pool *pgxpool.Pool,
	cfg *config.Config, database string, disk autonomy.DiskCapacityProvider,
	monitor *ha.Monitor) *runwayAdvisor {
	return newRunwayAdvisor(freeze, newWALCustodian(pool, cfg, database, disk),
		haReplicaGate(monitor))
}

// providerDiskCapacity is managed-cloud storage capacity for the WAL
// custodian, telling storage that grows without a bound apart.
type providerDiskCapacity struct{ cloud *cloudtel.Runtime }

func (p providerDiskCapacity) CapacityBytes(ctx context.Context) (int64, error) {
	bytes, err := p.cloud.CapacityBytes(ctx)
	if errors.Is(err, cloudtel.ErrUnbounded) {
		return 0, fmt.Errorf("%w: %w", autonomy.ErrDiskCapacityUnbounded, err)
	}
	return bytes, err
}

// diskCapacity is the database's provider storage capacity: a nil
// interface without managed-cloud telemetry, never a typed nil runtime.
func (rt *databaseRuntime) diskCapacity() autonomy.DiskCapacityProvider {
	if rt.cloud == nil {
		return nil
	}
	return providerDiskCapacity{cloud: rt.cloud}
}

// runwayCapacity is the provider capacity of the runway monitor and the
// forecaster (nil without managed-cloud telemetry: the declared capacity
// alone, as before).
func (rt *databaseRuntime) runwayCapacity() runway.CapacitySource {
	disk := rt.diskCapacity()
	if disk == nil {
		return nil
	}
	return func(ctx context.Context) (float64, string, bool) {
		bytes, err := disk.CapacityBytes(ctx)
		switch {
		case errors.Is(err, autonomy.ErrDiskCapacityUnbounded):
			return 0, runway.CapacityUnbounded, false
		case err != nil || bytes <= 0:
			return 0, "", false
		}
		return float64(bytes), runway.CapacityProviderTelemetry, true
	}
}

// forecastCapacity is the disk capacity /api/v1/forecasts/growth reports,
// resolved as forecast_disk_growth resolves it.
func (rt *databaseRuntime) forecastCapacity() func(context.Context) forecaster.Capacity {
	declared, src := rt.cfg.Forecaster.DiskCapacityBytes, rt.runwayCapacity()
	return func(ctx context.Context) forecaster.Capacity {
		return forecaster.ResolveCapacity(ctx, declared, src)
	}
}

// runwayMonitorOptions are the database's runway options with its
// provider storage capacity and, in fleet mode, the process's cluster
// owners (forecast-09).
func (rt *databaseRuntime) runwayMonitorOptions() runway.Options {
	opts := runwayOptions(rt.cfg, rt.spec.Name)
	opts.Capacity = rt.runwayCapacity()
	opts.Owners = rt.runwayOwners()
	return opts
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
		rt.runwayMonitorOptions(), logStructuredWrapper)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: runway monitor not started: %v", rt.spec.Name, err)
		return
	}
	rt.start(func() { m.Run(probes.WithCaller(rt.ctx, probes.CallerRunway)) })
	rt.note("runways")
}
