package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// A VACUUM by a role without the privilege succeeds with only a WARNING
// ("skipping ..."), so a statement that returned is no proof it ran: the
// verifier compares vacuum_count before and after (vacuum-06).
const (
	// metricVacuumCount is pg_stat_user_tables.vacuum_count.
	metricVacuumCount = "vacuum_count"
	// vacuumCountKey is the before_state key of vacuum_count before a VACUUM.
	vacuumCountKey = "vacuum_count_before"
	// vacuumSkippedReason leads the failure of a VACUUM the server skipped.
	vacuumSkippedReason = "VACUUM skipped (no privilege)"
)

// vacuumCountBaseline records the table's vacuum_count before a VACUUM.
func (e *Executor) vacuumCountBaseline(ctx context.Context, table string, before map[string]any) {
	count, err := readMaintenanceMetric(ctx, e.pool, metricVacuumCount, table)
	if err != nil {
		e.logFn("executor", "vacuum_count baseline for %s unavailable: %v", table, err)
		return
	}
	before[vacuumCountKey] = count
}

// vacuumSkipped reports a vacuum_count that did not advance; an unreadable
// value (negative) is no evidence either way.
func vacuumSkipped(before, after float64) bool {
	return before >= 0 && after >= 0 && after <= before
}

// settleSkippedVacuum fails a VACUUM the server skipped: vacuum_count did
// not advance and the connected role lacks the privilege to vacuum the
// table. A privileged role's VACUUM whose statistics report is late or
// lost (PostgreSQL 14's collector drops reports under load) is left to the
// metric judgement. It reports whether it settled the action.
func (e *Executor) settleSkippedVacuum(
	ctx context.Context, o verify.Outcome, state immediateState,
) bool {
	if o.Class != verify.ClassVacuum || state.VacuumCount == nil || state.Table == "" {
		return false
	}
	before := *state.VacuumCount
	after := settledMaintenanceMetric(ctx, e.pool, metricVacuumCount, state.Table, before)
	if !vacuumSkipped(before, after) {
		return false
	}
	readCtx, cancel := context.WithTimeout(ctx, maintenancePrivilegeTimeout)
	p, err := readMaintenancePrivilege(readCtx, e.pool, state.Table, "vacuum_table")
	cancel()
	if err != nil {
		e.logFn("verify", "action %d: vacuum_count of %s did not advance and the "+
			"privilege re-check failed: %v", o.ActionLogID, state.Table, err)
		return false
	}
	if p.Allowed {
		return false
	}
	reason := fmt.Sprintf("%s: vacuum_count of %s stayed at %.0f; %s",
		vacuumSkippedReason, state.Table, after, p.Detail)
	o.Verdict, o.Reason = verify.OutcomeUnverifiable, reason
	end := time.Now()
	o.WindowEnd = &end
	if !setMonitoredOutcome(ctx, e.pool, o.ActionLogID, "failed", reason) {
		e.logFn("verify", "action %d changed state; skipped VACUUM not recorded",
			o.ActionLogID)
		return true
	}
	e.logFn("verify", "action %d: failed (%s)", o.ActionLogID, reason)
	recordVerdict(ctx, e.pool, o, e.logFn)
	if _, err := finalizeActionVerification(ctx, e.pool, o.ActionLogID, "failed",
		reason); err != nil {
		e.logFn("verify", "finalize verification of action %d: %v", o.ActionLogID, err)
	}
	return true
}
