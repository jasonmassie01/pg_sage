package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// M5 approved actions as M7 ledger outcomes. A cancel_backend is
// approval-only (a human approved each run), so a run is an L2 outcome of
// its family's backend_cancel pair: a recovery pg_sage caused is a
// verified recovery (promotion evidence), a failed run or a recovery that
// did not happen is not recovered. Recoveries someone else caused, or whose
// cause is unknown, inconclusive and unfinished verifications, refusals
// (nothing ran) and runs without an action_log row are not recorded.

// actionOutcomeLookback is how far back runs are read: the ledger's safety
// window, so no run that still matters is missed.
const actionOutcomeLookback = 30 * 24 * time.Hour

// actionOutcomeSource is the M5 action service's outcome read surface.
type actionOutcomeSource interface {
	Outcomes(ctx context.Context, since time.Time, limit int) ([]sreaction.ActionOutcome,
		error)
}

// actionLedgerOutcome maps one M5 run; ok is false when it is not recorded.
func actionLedgerOutcome(database string, o sreaction.ActionOutcome) (earned.Outcome,
	bool) {
	family := earned.Family(o.Family)
	if o.Class != sreaction.ActionCancelBackend || o.ActionLogID <= 0 ||
		!earned.KnownFamily(family) || !earned.Applicable(family, earned.ClassBackendCancel) {
		return earned.Outcome{}, false
	}
	var result string
	switch {
	case o.State == sreaction.ProposalFailed:
		result = earned.ResultNotRecovered
	case o.State != sreaction.ProposalExecuted:
		return earned.Outcome{}, false
	case o.Recovery == sreaction.RecoveryNotRecovered:
		result = earned.ResultNotRecovered
	case o.Recovery == sreaction.RecoveryRecovered &&
		o.Attribution == sreaction.AttributionSage:
		result = earned.ResultVerifiedRecovery
	default:
		return earned.Outcome{}, false
	}
	return earned.Outcome{Database: database, ActionLogID: o.ActionLogID, Family: family,
		Class: earned.ClassBackendCancel, Level: earned.L2, Result: result,
		Source: earned.SourceExecutor, Actor: earned.ActorPgSage,
		Detail: fmt.Sprintf("M5 proposal %s: %s, recovery %q (%s)", o.ProposalID, o.State,
			o.Recovery, o.Attribution)}, true
}

// actionOutcomeFeed records a database's M5 runs in the ledger.
type actionOutcomeFeed struct {
	source   actionOutcomeSource
	ledger   *earned.Service
	database string
}

// RunOnce records every decided run not yet recorded; it returns how many
// were recorded now (the ledger keeps each action once).
func (f actionOutcomeFeed) RunOnce(ctx context.Context) (int, error) {
	outs, err := f.source.Outcomes(ctx, time.Now().Add(-actionOutcomeLookback), 500)
	if err != nil {
		return 0, fmt.Errorf("read M5 action outcomes of %s: %w", f.database, err)
	}
	recorded := 0
	var errs []error
	for _, o := range outs {
		out, ok := actionLedgerOutcome(f.database, o)
		if !ok {
			continue
		}
		inserted, err := f.ledger.RecordOutcomeOnce(ctx, out)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if inserted {
			recorded++
		}
	}
	return recorded, errors.Join(errs...)
}

// startActionOutcomeFeed runs the feed beside the reconciler once the M5
// action service exists.
func (rt *databaseRuntime) startActionOutcomeFeed() {
	entry, ok := processAutonomy().registry.Lookup(rt.spec.Name)
	if !ok || rt.sreActions == nil {
		return
	}
	feed := actionOutcomeFeed{source: rt.sreActions, ledger: entry.Service,
		database: rt.spec.Name}
	name := rt.spec.Name
	rt.start(func() {
		every(rt.ctx, rt.cfg.SRE.Autonomy.ReconcileInterval(), func(ctx context.Context) {
			if _, err := feed.RunOnce(ctx); err != nil {
				logWarn("autonomy", "db %q: record M5 action outcomes: %v", name, err)
			}
		})
	})
}
