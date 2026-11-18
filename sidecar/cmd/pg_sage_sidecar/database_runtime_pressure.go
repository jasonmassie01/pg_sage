package main

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// pressureChecker is the database's check_pressure (guard-01a): bounded
// catalog reads on its own pool, free storage from managed-cloud
// telemetry when there is any, and limits from its standing policy's
// pressure block over defaults derived from the alert thresholds. Nil
// without a pool.
func (rt *databaseRuntime) pressureChecker() *pressure.Checker {
	if rt.spec.Pool == nil {
		return nil
	}
	alerts := func() pressure.Alerts {
		config.RLockForHotReload()
		defer config.RUnlockForHotReload()
		return pressureAlerts(rt.cfg)
	}
	opts := pressure.Options{Database: rt.spec.Name, Alerts: alerts(),
		Source: pressure.NewSQLSource(rt.spec.Pool, pressure.SQLOptions{})}
	if rt.cloud != nil {
		opts.Storage = rt.cloud
	}
	store, scope := rt.policyStoreScope()
	opts.Limits = pressureLimitsFrom(func(ctx context.Context) (policy.Policy, error) {
		return store.Current(ctx, scope)
	}, alerts)
	return pressure.NewChecker(opts)
}

// pressureAlerts are the alert thresholds the pressure defaults halve.
func pressureAlerts(cfg *config.Config) pressure.Alerts {
	if cfg == nil {
		return pressure.Alerts{}
	}
	return pressure.Alerts{ReplicaLagSeconds: float64(cfg.RCA.ReplicationLagThresholdS),
		XIDWarning:        float64(cfg.Analyzer.XIDWraparoundWarning),
		MinFreeStoragePct: cloudLimits(cfg.CloudTelemetry).MinFreeStoragePct}
}

// pressureLimitsFrom reads the standing policy's pressure block on each
// read set; a failure is the checker's stop.
func pressureLimitsFrom(read func(context.Context) (policy.Policy, error),
	alerts func() pressure.Alerts) func(context.Context) (pressure.Limits, error) {
	return func(ctx context.Context) (pressure.Limits, error) {
		current, err := read(ctx)
		if err != nil {
			return pressure.Limits{}, fmt.Errorf("read the standing policy: %w", err)
		}
		doc, err := policy.DocumentFromPolicy(current)
		if err != nil {
			return pressure.Limits{}, fmt.Errorf("parse the standing policy: %w", err)
		}
		return pressure.LimitsFromPolicy(doc.Pressure, alerts()), nil
	}
}
