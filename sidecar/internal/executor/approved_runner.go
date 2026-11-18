package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/store"
)

// ApprovedRun is the outcome of running one approved queue item.
type ApprovedRun struct {
	ActionLogID int64
	// VerificationStatus is the queue item's verification state after the
	// run ("" lets the caller decide).
	VerificationStatus string
}

// ApprovedActionRunner runs approved queue items it owns (a Sage SRE
// proposal's approval item) instead of the manual SQL path.
type ApprovedActionRunner interface {
	Owns(action store.QueuedAction) bool
	RunApproved(ctx context.Context, action store.QueuedAction,
		approvedBy int) (ApprovedRun, error)
}

// PlanStepIdentityPrefix starts the identity key of a plan step's queue
// item (plan_step:<plan>:<step>, plan-02a). Such an item runs only
// through the plan runner: with none registered it is refused.
const PlanStepIdentityPrefix = "plan_step:"

// PressureJobIdentityPrefix starts the identity key of a backfill job's
// cancel (pressure_job:<job>:<pid>:<backend_start>, guard-01b). Such an
// item runs only through the pressure runner, which rechecks the job, the
// pressure and the backend first: with none that owns it, it is refused.
const PressureJobIdentityPrefix = "pressure_job:"

// Errors of the approved-action runner list.
var (
	ErrInvalidRunner = errors.New("invalid approved-action runner")
	// ErrRunnerOverlap: two runners claim the same identity keys.
	ErrRunnerOverlap = errors.New("approved-action runners overlap")
	// ErrPlanStepOutsidePlan: a plan step's queue item reached the
	// executor without its plan engine (an Actions approval, a card, the
	// scheduled runner) and was refused.
	ErrPlanStepOutsidePlan = errors.New("a plan step runs only through its plan engine")
	// ErrPressureJobOutsideRunner: a backfill job's cancel reached the
	// executor without the pressure runner owning it and was refused.
	ErrPressureJobOutsideRunner = errors.New(
		"a backfill job's cancel runs only through its pressure runner")
)

// registeredRunner is one runner and the identity-key prefixes it owns.
type registeredRunner struct {
	name     string
	prefixes []string
	runner   ApprovedActionRunner
}

// claims reports whether key starts with one of the runner's prefixes.
func (r registeredRunner) claims(key string) bool {
	for _, p := range r.prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// approvedRunnerSlot guards the registered runners, in registration order.
type approvedRunnerSlot struct {
	mu      sync.RWMutex
	runners []registeredRunner
}

// AddApprovedActionRunner registers r for the approved items whose
// identity key starts with one of prefixes (and that r.Owns). Prefixes
// must be disjoint from every registered runner's: a key two runners
// could own is a startup error naming both, and nothing is registered.
func (e *Executor) AddApprovedActionRunner(name string, prefixes []string,
	r ApprovedActionRunner) error {
	if r == nil || strings.TrimSpace(name) == "" || len(prefixes) == 0 {
		return fmt.Errorf("%w %q: it needs a runner, a name and identity-key prefixes",
			ErrInvalidRunner, name)
	}
	for _, p := range prefixes {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%w %q: an empty identity-key prefix owns every item",
				ErrInvalidRunner, name)
		}
	}
	slot := &e.approvedRunner
	slot.mu.Lock()
	defer slot.mu.Unlock()
	for _, have := range slot.runners {
		for _, p := range prefixes {
			for _, q := range have.prefixes {
				if strings.HasPrefix(p, q) || strings.HasPrefix(q, p) {
					return fmt.Errorf("%w: runner %q (%q) and runner %q (%q) would both "+
						"own the same queue items", ErrRunnerOverlap, have.name, q, name, p)
				}
			}
		}
	}
	slot.runners = append(slot.runners, registeredRunner{name: name,
		prefixes: append([]string(nil), prefixes...), runner: r})
	return nil
}

// ownerOf is the registered runner that owns action, or nil.
func (e *Executor) ownerOf(action store.QueuedAction) ApprovedActionRunner {
	slot := &e.approvedRunner
	slot.mu.RLock()
	runners := slot.runners
	slot.mu.RUnlock()
	for _, r := range runners {
		if r.claims(action.IdentityKey) && r.runner.Owns(action) {
			return r.runner
		}
	}
	return nil
}

// approvedRunnerCount is the number of registered runners.
func (e *Executor) approvedRunnerCount() int {
	slot := &e.approvedRunner
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	return len(slot.runners)
}

// RunApprovedAction runs an approved queue item for its approver: items a
// registered runner owns go to it, every other item keeps the manual path
// (ExecuteManual against its finding), except a plan step, a backfill
// job's cancel or an SRE maintenance item no runner owns, which is refused.
func (e *Executor) RunApprovedAction(ctx context.Context, action store.QueuedAction,
	approvedBy int) (ApprovedRun, error) {
	if approvedBy <= 0 {
		return ApprovedRun{}, ErrBackendApprovalRequired
	}
	if actionID, ok := restoreQueueActionID(action); ok {
		return e.runApprovedDropRestore(ctx, action, actionID)
	}
	if IsRevertCard(action) {
		return e.runApprovedRevert(ctx, action)
	}
	ctx = withQueuedPrincipal(ctx, action)
	if runner := e.ownerOf(action); runner != nil {
		return runner.RunApproved(ctx, action, approvedBy)
	}
	if strings.HasPrefix(action.IdentityKey, PlanStepIdentityPrefix) {
		return ApprovedRun{}, fmt.Errorf("queue item %d (%s): %w", action.ID,
			action.IdentityKey, ErrPlanStepOutsidePlan)
	}
	if strings.HasPrefix(action.IdentityKey, PressureJobIdentityPrefix) {
		return ApprovedRun{}, fmt.Errorf("queue item %d (%s): %w", action.ID,
			action.IdentityKey, ErrPressureJobOutsideRunner)
	}
	if strings.HasPrefix(action.IdentityKey, "sre_maintenance:") {
		return ApprovedRun{}, fmt.Errorf("SRE maintenance requires its typed runner")
	}
	ctx = e.withQueueLadderOf(withProposalOrigin(ctx, action), action.ID)
	id, err := e.ExecuteManual(ctx, action.FindingID, action.ProposedSQL,
		action.RollbackSQL, &approvedBy)
	return ApprovedRun{ActionLogID: id}, err
}
