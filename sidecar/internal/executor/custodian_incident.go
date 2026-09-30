package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/value"
)

// freezeHorizon is one measurement of a table's wraparound runway.
type freezeHorizon struct {
	xidAge, xidMax   int64
	mxidAge, mxidMax int64
}

func (h freezeHorizon) urgency(thresholds freeze.Thresholds) freeze.Urgency {
	return freeze.TableUrgency(h.xidAge, h.xidMax, h.mxidAge, h.mxidMax, thresholds)
}

func (e *Executor) readFreezeHorizon(
	ctx context.Context, target string,
) (freezeHorizon, error) {
	var horizon freezeHorizon
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT age(relfrozenxid)::bigint,
		current_setting('autovacuum_freeze_max_age')::bigint,
		mxid_age(relminmxid)::bigint,
		current_setting('autovacuum_multixact_freeze_max_age')::bigint
		FROM pg_class WHERE oid=to_regclass($1)`, target).Scan(
		&horizon.xidAge, &horizon.xidMax, &horizon.mxidAge, &horizon.mxidMax)
	if err != nil {
		return freezeHorizon{}, fmt.Errorf("read freeze horizon of %s: %w", target, err)
	}
	return horizon, nil
}

// measured reports whether both maxima were read. An unmeasured horizon
// reads as red (fail closed), which must not count as evidence of danger.
func (h freezeHorizon) measured() bool { return h.xidMax > 0 && h.mxidMax > 0 }

// freezeNearMiss reports whether a freeze earned incident credit: the
// table was measured inside the red buffer before the action and is
// outside the amber buffer after it. Anything less is routine maintenance
// (toil).
func freezeNearMiss(
	before *freezeHorizon, after freezeHorizon, thresholds freeze.Thresholds,
) bool {
	return before != nil && before.measured() &&
		before.urgency(thresholds) == freeze.UrgencyRed &&
		after.urgency(thresholds) == freeze.UrgencyGreen
}

// creditFreezeIncident records a wraparound near miss for a verified
// freeze. Credit is best-effort bookkeeping: a failure is logged and never
// fails the action that already succeeded.
func (r *custodianRun) creditFreezeIncident(ctx context.Context, actionID int64) {
	e := r.executor
	if r.proposal.Feature != "freeze" || r.baseline.horizon == nil || actionID <= 0 {
		return
	}
	thresholds := freeze.BufferThresholds(e.cfg.Custodian.Freeze.RedBufferPct)
	if r.baseline.horizon.urgency(thresholds) != freeze.UrgencyRed {
		return
	}
	target := r.proposal.TargetObjects[0]
	after, err := e.readFreezeHorizon(ctx, target)
	if err != nil {
		e.logFn("executor", "incident credit for action %d withheld: %v", actionID, err)
		return
	}
	if !freezeNearMiss(r.baseline.horizon, after, thresholds) {
		return
	}
	_, err = value.NewService(value.NewPostgresRepository(e.pool)).
		CreditAvoidedIncident(ctx, value.AvoidedIncident{
			ActionID: actionID, Kind: value.IncidentXIDWraparound,
			Severity: value.SeverityNearMiss, OccurredAt: time.Now().UTC(),
		})
	if err != nil && !errors.Is(err, value.ErrIncidentAlreadyCredited) {
		e.logFn("executor", "record wraparound near miss for action %d on %s: %v",
			actionID, target, err)
	}
}
