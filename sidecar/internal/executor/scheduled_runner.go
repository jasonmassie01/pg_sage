package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approve now, run in the next window (policy-04). An operator approval
// that only the maintenance window withholds is scheduled for the next
// opening. Each cycle the runner asks the gate again about each due row
// under the current policy (stops, trust, narrowed classes and the start
// margin all bind), then claims it (one sidecar wins) and runs it through
// RunApprovedAction. A withheld row returns to pending before it is
// claimed, so its recommendation is never left approved: the executor
// cycle would run an approved recommendation on its own. The approver is
// told whenever a row returns to pending.

// maxScheduledRunsPerCycle bounds the approvals one cycle starts.
const maxScheduledRunsPerCycle = 8

var (
	// ErrApprovalRunsNow: the gate would run the approval now; approve it.
	ErrApprovalRunsNow = errors.New("the action can run now; approve it instead")
	// ErrNotWindowOnly: something other than the window withholds it.
	ErrNotWindowOnly = errors.New("the action is not withheld by the maintenance window alone")
)

// ScheduledApproval is an approval scheduled for its next window.
type ScheduledApproval struct {
	Action store.QueuedAction
	Start  policy.WindowStart
}

// ScheduleApproval schedules userID's approval of queue item queueID for
// the next window, when the window is its only refusal. A non-empty
// expectSQL must still be the item's proposed SQL.
func (e *Executor) ScheduleApproval(ctx context.Context, as *store.ActionStore,
	queueID, userID int, expectSQL string) (ScheduledApproval, error) {
	if as == nil {
		return ScheduledApproval{}, errors.New("schedule approval: no action store")
	}
	action, err := as.GetByID(ctx, queueID)
	if err != nil {
		return ScheduledApproval{}, fmt.Errorf("schedule action %d: %w", queueID, err)
	}
	evidence, err := as.FindingEvidencePresent(ctx, action.FindingID)
	if err != nil {
		evidence = true // as the approval path: the lifecycle check decides
	}
	readiness := e.ApprovalReadinessWithEvidence(*action, time.Now().UTC(), evidence)
	return e.ScheduleReady(ctx, as, *action, readiness, userID, expectSQL)
}

// ScheduleReady is ScheduleApproval for an item whose readiness the caller
// already evaluated.
func (e *Executor) ScheduleReady(ctx context.Context, as *store.ActionStore,
	action store.QueuedAction, readiness ApprovalReadiness, userID int,
	expectSQL string) (ScheduledApproval, error) {
	if readiness.Eligible {
		return ScheduledApproval{}, ErrApprovalRunsNow
	}
	if !readiness.PolicyKnown || !readiness.Policy.WindowOnly {
		return ScheduledApproval{}, fmt.Errorf("%w: %s", ErrNotWindowOnly,
			strings.TrimSpace(readiness.DeferReason))
	}
	start, err := e.NextApprovalWindow(ctx, action.ProposedSQL)
	if err != nil {
		return ScheduledApproval{}, fmt.Errorf("plan the next window for action %d: %w",
			action.ID, err)
	}
	var closes *time.Time
	if !start.ClosesAt.IsZero() {
		closes = &start.ClosesAt
	}
	scheduled, err := as.Schedule(ctx, action.ID, userID, expectSQL, start.At, closes)
	if err != nil {
		return ScheduledApproval{}, err
	}
	return ScheduledApproval{Action: *scheduled, Start: start}, nil
}

// NextApprovalWindow is when an operator approval of sql may next start.
func (e *Executor) NextApprovalWindow(ctx context.Context, sql string) (
	policy.WindowStart, error) {
	planner, ok := e.StandingPolicyGate().(policy.WindowPlanner)
	if !ok {
		return policy.WindowStart{}, errors.New("the standing policy gate cannot plan windows")
	}
	request, _ := operatorRequest(sql, 0, nil)
	return planner.NextWindowStart(ctx, request)
}

// RunScheduledApprovals settles scheduled approvals and returns how many
// it ran: stale schedules return to pending, then due ones are checked,
// claimed and run one at a time.
func (e *Executor) RunScheduledApprovals(ctx context.Context) int {
	if e == nil || e.pool == nil {
		return 0
	}
	as := store.NewActionStore(e.pool)
	e.returnStaleSchedules(ctx, as)
	ran := 0
	for range maxScheduledRunsPerCycle {
		due, err := as.NextDueSchedule(ctx)
		if errors.Is(err, store.ErrNoDueSchedule) {
			return ran
		}
		if err != nil {
			e.logFn("executor", "read due scheduled approvals: %v", err)
			return ran
		}
		executed, err := e.runDueSchedule(ctx, as, *due)
		if err != nil {
			e.logFn("executor", "scheduled approval %d: %v", due.ID, err)
			return ran
		}
		if executed {
			ran++
		}
	}
	return ran
}

// runDueSchedule asks the gate about one due schedule, then claims and
// runs it. It reports whether the action ran, and an error that should end
// this cycle's pass (a failed queue write, not a refusal or a lost race).
func (e *Executor) runDueSchedule(ctx context.Context, as *store.ActionStore,
	due store.QueuedAction) (bool, error) {
	preview := e.explainOperatorAction(ctx, due.ProposedSQL)
	if preview.Decision != PolicyDecisionExecute {
		return false, e.withholdSchedule(ctx, as, due, preview)
	}
	action, err := as.ClaimDueSchedule(ctx, due.ID)
	switch {
	case errors.Is(err, store.ErrNoDueSchedule):
		return false, nil // another sidecar claimed or settled it
	case errors.Is(err, store.ErrScheduleRevised) && action != nil:
		e.notifyScheduleReturned(ctx, *action)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("claim: %w", err)
	}
	return e.runClaimedSchedule(ctx, as, *action), nil
}

// withholdSchedule returns a schedule the gate refuses at its opening to
// pending, unclaimed, and tells the approver why.
func (e *Executor) withholdSchedule(ctx context.Context, as *store.ActionStore,
	due store.QueuedAction, preview ActionPolicyDecision) error {
	reason := "scheduled approval withheld at the window opening: " +
		humanPolicyReason(preview)
	returned, err := as.ReturnScheduledToPending(ctx, due.ID, reason)
	if errors.Is(err, store.ErrNotScheduled) {
		return nil // another sidecar claimed or settled it first
	}
	if err != nil {
		return fmt.Errorf("return to pending: %w", err)
	}
	e.notifyScheduleReturned(ctx, *returned)
	return nil
}

func (e *Executor) returnStaleSchedules(ctx context.Context, as *store.ActionStore) {
	returned, err := as.ReturnStaleSchedules(ctx)
	if err != nil {
		e.logFn("executor", "return stale scheduled approvals: %v", err)
	}
	for _, action := range returned {
		e.notifyScheduleReturned(ctx, action)
	}
}

// runClaimedSchedule runs one claimed schedule; RunApprovedAction
// authorizes it again as the approver's operator approval.
func (e *Executor) runClaimedSchedule(ctx context.Context, as *store.ActionStore,
	action store.QueuedAction) bool {
	approver := 0
	if action.DecidedBy != nil {
		approver = *action.DecidedBy
	}
	run, err := e.RunApprovedAction(ctx, action, approver)
	if err != nil {
		e.recordScheduledFailure(ctx, as, action, err)
		return false
	}
	verification := run.VerificationStatus
	if verification == "" {
		verification = "verified"
		if action.RollbackSQL != "" {
			verification = "monitoring"
		}
	}
	if err := as.MarkExecuted(ctx, action.ID, run.ActionLogID, verification); err != nil {
		e.logFn("executor", "mark scheduled approval %d executed: %v", action.ID, err)
	}
	return true
}

// recordScheduledFailure records a failed run on the row (never
// rescheduled) and tells the approver.
func (e *Executor) recordScheduledFailure(ctx context.Context, as *store.ActionStore,
	action store.QueuedAction, runErr error) {
	if err := as.MarkAttemptFailed(ctx, action.ID, runErr.Error(), time.Hour); err != nil {
		e.logFn("executor", "record scheduled approval %d failure: %v", action.ID, err)
	}
	e.logFn("executor", "scheduled approval %d failed: %v", action.ID, runErr)
	e.dispatchEvent(ctx, notify.ActionFailedEvent(scheduleTitle(action),
		action.ProposedSQL, e.databaseName, runErr.Error()))
}

// notifyScheduleReturned asks for a new approval of a schedule that
// returned to pending, naming the reason and who had approved it.
func (e *Executor) notifyScheduleReturned(ctx context.Context, action store.QueuedAction) {
	title := scheduleTitle(action) + " returned to approval: " + action.Reason
	e.dispatchEvent(ctx, notify.QueuedApprovalEvent(title, action.ProposedSQL,
		e.databaseName, action.ActionRisk, action.ID))
}

func scheduleTitle(action store.QueuedAction) string {
	title := fmt.Sprintf("Scheduled approval #%d", action.ID)
	if action.ScheduledBy != nil {
		title += fmt.Sprintf(" (approved by user %d)", *action.ScheduledBy)
	}
	return title
}
