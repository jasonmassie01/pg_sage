package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// refreshCollectionStatus uses only evidence owned by this runtime generation.
// Collection time is the sample time, never the time of a status poll.
func refreshCollectionStatus(ctx context.Context, inst *fleet.DatabaseInstance) {
	if inst.Collector != nil {
		applyCollectionSnapshot(inst, inst.Collector.LatestSnapshot())
	}
	if inst.Pool == nil || inst.SnapshotStatus().PGVersion != "" {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var version string
	if err := inst.Pool.QueryRow(queryCtx, "SHOW server_version").Scan(&version); err != nil {
		logWarn("fleet", "database %q: read PostgreSQL version: %v", inst.Name, err)
		return
	}
	inst.UpdateStatus(func(s *fleet.InstanceStatus) { s.PGVersion = version })
}

func applyCollectionSnapshot(inst *fleet.DatabaseInstance, snapshot *collector.Snapshot) {
	if snapshot == nil || snapshot.CollectedAt.IsZero() {
		return
	}
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		if snapshot.CollectedAt.Before(s.CollectorLastRun) {
			return
		}
		s.DatabaseSize = snapshot.System.DBSizeBytes
		s.CollectorLastRun = snapshot.CollectedAt
	})
}
