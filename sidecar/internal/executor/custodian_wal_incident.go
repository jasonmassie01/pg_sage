package executor

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/runway"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/value"
)

// Avoided-incident credit for a WAL bound (disk_full_slot). The disk
// runway is measured just before the bound (usage, retained WAL and the
// sampled fill trend) and again after it is verified; runway.WALBoundCredit
// decides. Credit is best-effort bookkeeping: a measurement failure
// withholds it and never fails the action.

// isWALBound reports a custodian WAL bound (an ALTER SYSTEM of
// max_slot_wal_keep_size).
func isWALBound(proposal CustodianProposal) bool {
	return proposal.Feature == "wal" && isAlterSystem(proposal.SQL)
}

func (e *Executor) diskRunner() *probes.Runner {
	return probes.NewRunner(e.pool, probes.Catalog(), nil)
}

// walDiskBaseline measures the disk runway before a WAL bound, only when
// a capacity is declared (without one nothing can be credited).
func (e *Executor) walDiskBaseline(ctx context.Context) *runway.DiskMeasure {
	if e.cfg == nil || e.cfg.Forecaster.DiskCapacityBytes <= 0 {
		return nil
	}
	m, err := runway.MeasureDisk(ctx, e.diskRunner(), e.cfg.SRE.Runways.Lookback())
	if err != nil {
		e.logFn("executor", "disk runway before the WAL bound not measured; "+
			"incident credit withheld: %v", err)
		return nil
	}
	return &m
}

// creditWALIncident records a disk_full_slot near miss for a verified WAL
// bound when the measured trend says it avoided one.
func (r *custodianRun) creditWALIncident(ctx context.Context, actionID int64) {
	e := r.executor
	if !isWALBound(r.proposal) || r.baseline.disk == nil || actionID <= 0 {
		return
	}
	after, err := runway.MeasureDisk(ctx, e.diskRunner(), e.cfg.SRE.Runways.Lookback())
	if err != nil {
		e.logFn("executor", "incident credit for action %d withheld: %v", actionID, err)
		return
	}
	rw := e.cfg.SRE.Runways
	ok, why := runway.WALBoundCredit(runway.CreditInput{
		Capacity:        float64(e.cfg.Forecaster.DiskCapacityBytes),
		ManagedProvider: isManagedProvider(e.cfg.CloudEnvironment),
		Before:          *r.baseline.disk, After: after, BoundBytes: after.SlotKeepBytes,
		Horizon: rw.DiskHorizon(), MinSamples: rw.MinSamples, MinSpan: rw.MinSpan(),
	})
	if !ok {
		e.logFn("executor", "WAL bound %d earned no disk_full_slot credit: %s", actionID, why)
		return
	}
	_, err = value.NewService(value.NewPostgresRepository(e.pool)).
		CreditAvoidedIncident(ctx, value.AvoidedIncident{
			ActionID: actionID, Kind: value.IncidentDiskFullSlot,
			Severity: value.SeverityNearMiss, OccurredAt: time.Now().UTC(),
		})
	if err != nil && !errors.Is(err, value.ErrIncidentAlreadyCredited) {
		e.logFn("executor", "record disk-full near miss for action %d: %v", actionID, err)
	}
}
