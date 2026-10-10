package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Agent governance in the executor (AGENTDB-SPEC §6.2.1, §6.2.7): cmd
// injects the decider into the standing gate, and an agent-originated
// change holds its principal active in the control database from the
// re-authorization until its target commit returns.

// PrincipalHold holds principalID active until release is called; it
// fails when the principal is not active (decide.HoldActive).
type PrincipalHold func(ctx context.Context, principalID string) (release func(), err error)

// ErrPrincipalInactive stops an agent's change whose principal is no
// longer active (frozen or retired since its authorization), or whose
// activity cannot be held.
var ErrPrincipalInactive = errors.New("agent principal is not active")

// WithAgentDecider installs agent governance in the standing gate, before
// or after the gate itself is installed.
func (e *Executor) WithAgentDecider(decider policy.AgentDecider) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.agentDecider = decider
}

// WithPrincipalHold installs the §6.2.7 hold. Without one, an agent's
// change fails closed.
func (e *Executor) WithPrincipalHold(hold PrincipalHold) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.principalHold = hold
}

// executorAgents is GateConfig.Agents for the executor's gates: it reads
// the installed decider at decision time, and without one caps agent
// requests at approval as a nil GateConfig.Agents would.
type executorAgents struct{ e *Executor }

func (a executorAgents) Decide(ctx context.Context, req policy.ActionRequest) (
	policy.Decision, int, bool) {
	a.e.policyMu.RLock()
	decider := a.e.agentDecider
	a.e.policyMu.RUnlock()
	if decider == nil {
		return policy.Decision{}, policy.AgentUngovernedCap, false
	}
	return decider.Decide(ctx, req)
}

// executeHeld runs an authorized intent's Execute under its principal's
// hold: from the re-authorization until the change (and its target
// COMMIT) returns. pg_sage's own changes, narrowing ones (the kill holds
// the row FOR UPDATE) and the unbound stdio agent's take no hold.
func (e *Executor) executeHeld(ctx context.Context, intent ActionIntent,
	decision ActionPolicyDecision) (int64, error) {
	ref := policy.RequestPrincipal(ctx, intent.Request)
	if ref == nil || ref.ID == "" || policy.IsNarrowing(intent.Request) {
		return intent.Execute(ctx, decision)
	}
	e.policyMu.RLock()
	hold := e.principalHold
	e.policyMu.RUnlock()
	if hold == nil {
		return 0, fmt.Errorf("%w: no principal hold is configured for agent %s",
			ErrPrincipalInactive, ref.ID)
	}
	release, err := hold(ctx, ref.ID)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPrincipalInactive, err)
	}
	defer release()
	return intent.Execute(ctx, decision)
}
