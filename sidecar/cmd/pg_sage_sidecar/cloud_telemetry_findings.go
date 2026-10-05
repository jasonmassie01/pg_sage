package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

const (
	categoryManagedDrift   = "managed_parameter_drift"
	categoryManagedStorage = "managed_storage_runway"
	// storageRunwayHorizonHours opens a storage finding a week ahead.
	storageRunwayHorizonHours = 7 * 24
)

// upsertManagedFindings records parameter drift (when the target was
// resolved) and the managed-storage runway (when telemetry is fresh).
// Unknown state leaves the open findings as they are.
func upsertManagedFindings(ctx context.Context, pool *pgxpool.Pool, driftKnown bool,
	drift []managedparam.Drift, cloud *cloudtel.Runtime) error {
	if driftKnown {
		if err := replaceFindings(ctx, pool, categoryManagedDrift,
			driftFindings(drift)); err != nil {
			return err
		}
	}
	if cloud == nil {
		return nil
	}
	st := cloud.Status()
	if !st.Available {
		return nil
	}
	var findings []analyzer.Finding
	if f, ok := storageFinding(st); ok {
		findings = append(findings, f)
	}
	return replaceFindings(ctx, pool, categoryManagedStorage, findings)
}

func replaceFindings(ctx context.Context, pool *pgxpool.Pool, category string,
	findings []analyzer.Finding) error {
	active := map[string]bool{}
	for _, f := range findings {
		active[f.ObjectIdentifier] = true
	}
	if len(findings) > 0 {
		if err := analyzer.UpsertFindings(ctx, pool, findings); err != nil {
			return fmt.Errorf("upsert %s findings: %w", category, err)
		}
	}
	if err := analyzer.ResolveCleared(ctx, pool, active, category); err != nil {
		return fmt.Errorf("resolve %s findings: %w", category, err)
	}
	return nil
}

func driftFindings(drift []managedparam.Drift) []analyzer.Finding {
	out := make([]analyzer.Finding, 0, len(drift))
	for _, d := range drift {
		severity := "info"
		advice := "Reboot the instance in a maintenance window to apply it."
		switch d.Kind {
		case managedparam.DriftMismatch:
			severity = "warning"
			advice = "Check that the parameter group is attached and in sync, or revert " +
				"the console change."
		case managedparam.DriftOverridden:
			advice = "A database, role or session setting takes precedence; remove it " +
				"if the provider value should apply."
		}
		out = append(out, analyzer.Finding{Category: categoryManagedDrift, Severity: severity,
			ObjectType: "configuration", ObjectIdentifier: "parameter:" + d.Parameter,
			Title: fmt.Sprintf("%s: provider value %s is not running (%s)", d.Parameter,
				d.Configured, d.Kind),
			Detail: map[string]any{"parameter": d.Parameter, "configured": d.Configured,
				"running": d.Running, "kind": d.Kind},
			Recommendation: d.Detail + ". " + advice})
	}
	return out
}

// storageFinding opens when managed storage runs out within a week
// (critical inside the admission floor).
func storageFinding(st cloudtel.Status) (analyzer.Finding, bool) {
	r := st.Runway
	if !r.Known || r.Hours < 0 || r.Hours >= storageRunwayHorizonHours {
		return analyzer.Finding{}, false
	}
	severity := "warning"
	if r.Hours < st.Limits.MinStorageRunwayHours {
		severity = "critical"
	}
	return analyzer.Finding{Category: categoryManagedStorage, Severity: severity,
		ObjectType: "storage", ObjectIdentifier: "storage:" + st.Resource,
		Title: fmt.Sprintf("Managed storage of %s runs out in %.0f hours", st.Resource,
			r.Hours),
		Detail: map[string]any{"runway_hours": r.Hours, "points": r.Points,
			"consumed_bytes_per_hour": -r.SlopeBytesPerHour, "resource": st.Resource},
		Recommendation: "Free storage is falling at its recent rate: raise the allocated " +
			"storage or the autoscaling maximum, or remove data. Autonomous index builds " +
			"wait while the runway is below the floor."}, true
}
