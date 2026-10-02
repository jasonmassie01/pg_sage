package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

var (
	// ErrActionWithheld wraps every verdict other than execute.
	ErrActionWithheld = errors.New("action withheld by policy")
	// ErrDDLSlotUnavailable reports that every DDL slot was busy for an
	// intent that does not wait for one.
	ErrDDLSlotUnavailable = errors.New("DDL concurrency limit reached")
)

// applyGrace lets PostgreSQL's statement_timeout fire before the execution
// deadline, so a timed-out CONCURRENTLY build fails cleanly instead of
// being cancelled from the client side.
const applyGrace = time.Minute

// ActionIntent is one change routed through Apply, the single execution
// pipeline shared by the executor cycle, operator actions, custodians,
// verified indexes and retention (G4-I07).
type ActionIntent struct {
	// Request is what the standing gate authorizes, before and after the
	// lease and slot waits.
	Request policy.ActionRequest
	// Authorize replaces the gate request for kinds with their own
	// authorization (operator approvals). It runs at both authorization
	// points.
	Authorize func(context.Context) (ActionPolicyDecision, error)
	// Lease is the finding whose DDL takes a change lease; nil takes none.
	Lease *analyzer.Finding
	// WaitForSlot blocks for a DDL slot; otherwise a busy executor skips.
	WaitForSlot bool
	// SlotHeld means the caller already holds a DDL slot for the whole
	// request: an operator action waits for it, bounded by the HTTP
	// request, before anything else.
	SlotHeld bool
	// AuthorizeOnly stops after the first authorization: the caller runs the
	// change itself (retention's bounded delete).
	AuthorizeOnly bool
	// Admit is an optional precondition checked after the authorization
	// (load admission records its verdict in that decision).
	Admit func(ctx context.Context, decisionID int64) error
	// Execute runs and records the change under the execution deadline and
	// returns the action_log id. It receives the re-authorized decision,
	// whose policy lock ceiling caps its statements (lockOption).
	Execute func(ctx context.Context, decision ActionPolicyDecision) (int64, error)
	// Verify post-checks a recorded change; nil when Execute starts its own
	// durable verification.
	Verify func(ctx context.Context, actionID int64) error
	// Refused records a failure after the authorization (a denied lease).
	Refused func(ctx context.Context, decisionID int64, err error)
}

// WithheldError reports a verdict other than execute. Reauthorized marks a
// refusal by the re-authorization that follows the lease and slot waits.
type WithheldError struct {
	Decision     ActionPolicyDecision
	Reauthorized bool
}

func (e *WithheldError) Error() string {
	stage := "authorization"
	if e.Reauthorized {
		stage = "re-authorization"
	}
	return fmt.Sprintf("%s: %s refused %s", ErrActionWithheld, stage,
		humanPolicyReason(e.Decision))
}

func (e *WithheldError) Unwrap() error { return ErrActionWithheld }

// Apply is the one execution pipeline: authorize → lease → DDL slot →
// re-authorize → execute and record under a mandatory deadline → verify.
// The re-authorization follows every wait, so an emergency stop or policy
// change made while the action waited stops it. It returns the action_log
// id; a policy refusal is a *WithheldError.
func (e *Executor) Apply(ctx context.Context, intent ActionIntent) (int64, error) {
	waitCtx, cancelWait := context.WithTimeout(ctx, e.applyTimeout())
	defer cancelWait()
	first, err := e.authorizeIntent(waitCtx, intent, false)
	if err != nil || intent.AuthorizeOnly {
		return 0, err
	}
	release, err := e.prepareIntent(waitCtx, intent, first.DecisionID)
	if err != nil {
		return 0, err
	}
	defer release()
	runCtx, cancelRun := context.WithTimeout(ctx, e.applyTimeout())
	defer cancelRun()
	// The re-authorization runs under this action's own change lease, which
	// the earned-autonomy ledger must not count as a concurrent writer.
	intent.Request.LeaseHeld = leaseTaken(intent, first.DecisionID)
	final, err := e.authorizeIntent(runCtx, intent, true)
	if err != nil {
		return 0, err
	}
	actionID, err := intent.Execute(runCtx, final)
	if err != nil || intent.Verify == nil {
		return actionID, err
	}
	return actionID, intent.Verify(runCtx, actionID)
}

// authorizeIntent asks the standing gate, the only authority. No gate
// fails closed.
func (e *Executor) authorizeIntent(
	ctx context.Context, intent ActionIntent, reauthorize bool,
) (ActionPolicyDecision, error) {
	if intent.Authorize != nil {
		return intent.Authorize(ctx)
	}
	gate := e.StandingPolicyGate()
	decision := ActionPolicyDecision{
		Decision: PolicyDecisionBlocked, RiskTier: "unknown",
		BlockedReason: reasonNoStandingPolicy,
	}
	if gate != nil {
		decision = standingPolicyDecision(gate.Authorize(ctx, intent.Request))
	}
	if decision.Decision != PolicyDecisionExecute {
		return decision, &WithheldError{Decision: decision, Reauthorized: reauthorize}
	}
	return decision, nil
}

// prepareIntent runs the admission check, takes the change lease and a DDL
// slot, and returns their release.
func (e *Executor) prepareIntent(
	ctx context.Context, intent ActionIntent, decisionID int64,
) (func(), error) {
	if intent.Admit != nil {
		if err := intent.Admit(ctx, decisionID); err != nil {
			return nil, err
		}
	}
	releaseLease := func() {}
	if intent.Lease != nil {
		var err error
		releaseLease, err = e.acquireDDLLease(ctx, *intent.Lease, decisionID)
		// A lease held by another writer parks the action (D1): recorded in
		// the ledger, never as a failed action.
		parked := e.parkLeaseConflict(ctx, *intent.Lease, decisionID, err)
		if err != nil {
			if intent.Refused != nil && !parked {
				intent.Refused(ctx, decisionID, err)
			}
			return nil, fmt.Errorf("acquire change lease: %w", err)
		}
	}
	releaseSlot, err := e.intentSlot(ctx, intent)
	if err != nil {
		releaseLease()
		return nil, err
	}
	return func() { releaseSlot(); releaseLease() }, nil
}

func (e *Executor) intentSlot(ctx context.Context, intent ActionIntent) (func(), error) {
	switch {
	case intent.SlotHeld:
		return func() {}, nil
	case intent.WaitForSlot:
		return e.acquireDDLSlot(ctx)
	default:
		return e.tryDDLSlot()
	}
}

// tryDDLSlot takes a DDL slot only when one is free.
func (e *Executor) tryDDLSlot() (func(), error) {
	if e.ddlSem == nil {
		return func() {}, nil
	}
	select {
	case e.ddlSem <- struct{}{}:
		return func() { <-e.ddlSem }, nil
	default:
		return nil, ErrDDLSlotUnavailable
	}
}

// ddlTimeout is the statement timeout for executor DDL. It is never
// unlimited: an unset safety.ddl_timeout_seconds uses the default.
func (e *Executor) ddlTimeout() time.Duration {
	cfg, _, _ := e.policySnapshot()
	if cfg != nil && cfg.Safety.DDLTimeout() > 0 {
		return cfg.Safety.DDLTimeout()
	}
	return time.Duration(config.DefaultDDLTimeoutSeconds) * time.Second
}

// applyTimeout bounds each stage of Apply: the statement timeout plus a
// grace period for the server-side timeout to report first.
func (e *Executor) applyTimeout() time.Duration {
	return e.ddlTimeout() + applyGrace
}

// lockOption is the lock_timeout for one statement under an authorized
// decision: safety.lock_timeout_ms, capped by the policy's
// lock_duration_ceiling_ms for in-transaction statements (D1). Every
// statement an intent runs takes it, so the ceiling holds on every path
// through Apply.
func (e *Executor) lockOption(sql string, decision ActionPolicyDecision) DDLOption {
	return WithLockTimeout(e.lockTimeoutMS(sql, decision))
}

func (e *Executor) lockTimeoutMS(sql string, decision ActionPolicyDecision) int {
	safety := config.DefaultLockTimeoutMs
	if cfg, _, _ := e.policySnapshot(); cfg != nil {
		safety = cfg.Safety.LockTimeout()
	}
	return ddlLockTimeoutMS(sql, safety, decision.LockCeilingMS)
}
