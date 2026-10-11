package policy

import "context"

// Agent-originated requests (spec §6.2). policy imports no internal
// package, so it defines the decider interface and cmd injects the
// implementation (internal/agentguard/decide), as with GateConfig.Autonomy
// and GateConfig.Facts.

// PrincipalRef names the agent a request comes from.
type PrincipalRef struct {
	// ID is the principal ("agp_…"); "" is an agent with no principal
	// (the stdio client without mcp.stdio_principal), which governance
	// treats as unsponsored.
	ID string
	// SponsorID is the sponsor's sage.users id; 0 = none.
	SponsorID int
	// TaskID is a trusted task id (an E2 token claim) or "".
	TaskID string
	// OnBehalfOf is the human subject of a token-exchange act claim, or "".
	OnBehalfOf string
	// Tool is the MCP tool the request was built from, or "" for an agent
	// request that came another way (§6.2.6 maps tools to classes).
	Tool string
}

// AgentDecider is agent governance's part of the gate (§6.2.2 A4).
type AgentDecider interface {
	// Decide returns the agent-specific outcome. stop=true means the
	// decision is final (a block, or a park that ends evaluation);
	// otherwise maxLevel (0-3) caps the remaining evaluation.
	Decide(ctx context.Context, req ActionRequest) (d Decision, maxLevel int, stop bool)
}

// Agent verdict reasons owned by the gate (§6.2.3). The D-step reasons
// (agent_frozen, agent_env_ceiling, ...) come from the decider.
const (
	// ReasonAgentLevel0 blocks a request whose ledger level is L0.
	ReasonAgentLevel0 Reason = "agent_level0"
	// ReasonAgentProposalRecorded records an L1 agent request as a
	// proposal (verdict observe_only).
	ReasonAgentProposalRecorded Reason = "agent_proposal_recorded"
	// ReasonAgentL3Envelope executes an L3 request inside its envelope.
	ReasonAgentL3Envelope Reason = "agent_l3_envelope"
)

// AgentUngovernedCap is the level an agent request is capped at when
// GateConfig.Agents is nil (governance off).
const AgentUngovernedCap = 2

type principalRefKey struct{}

// WithPrincipalRef returns ctx carrying the agent a request comes from.
// Every ActionRequest the gate sees under ctx is then agent-originated.
func WithPrincipalRef(ctx context.Context, ref PrincipalRef) context.Context {
	return context.WithValue(ctx, principalRefKey{}, ref)
}

// PrincipalRefFromContext returns the agent bound to ctx, if any.
func PrincipalRefFromContext(ctx context.Context) (PrincipalRef, bool) {
	ref, ok := ctx.Value(principalRefKey{}).(PrincipalRef)
	return ref, ok
}

// withContextPrincipal fills req.Principal from ctx when it is nil, so a
// request built anywhere below an agent's MCP call carries the agent.
func withContextPrincipal(ctx context.Context, req ActionRequest) ActionRequest {
	if req.Principal != nil {
		return req
	}
	if ref, ok := PrincipalRefFromContext(ctx); ok {
		req.Principal = &ref
	}
	return req
}

// agentDecision is A4-A6 for an agent-originated request (§6.2.2): the
// decider's D-steps, then the level (decider cap, operator ceiling,
// rollback-class cap) mapped to a verdict (§6.2.3). A1-A3 already ran.
func (gate *authorizationGate) agentDecision(
	ctx context.Context, runtime RuntimeState, req ActionRequest,
) Decision {
	level := AgentUngovernedCap
	if gate.config.Agents != nil {
		decision, maxLevel, stop := gate.config.Agents.Decide(ctx, req)
		if stop {
			return agentStop(req, decision)
		}
		level = maxLevel
	}
	if agentOperatorCeiling(runtime.TrustLevel) == 0 {
		return blocked(ReasonUnknownTrustLevel, runtime.TrustLevel)
	}
	if req.OperatorApproved && level >= 2 {
		// The approval satisfies L2; the operator ceiling, the change-class
		// allowlist and the windows still bind (operatorDecision).
		return gate.awaitVerification(ctx, req, gate.operatorDecision(ctx, runtime, req))
	}
	level = min(level, agentOperatorCeiling(runtime.TrustLevel),
		agentRollbackCap(req.Contract.RollbackClass))
	if runtime.ExecutionMode == ExecutionManual {
		level = min(level, 1)
	}
	return gate.agentLevelDecision(ctx, req, level)
}

// agentLevelDecision maps a level to a verdict. G1 has no L3 envelope
// (§6.11, from G2), so L3 falls back to L2.
func (gate *authorizationGate) agentLevelDecision(
	ctx context.Context, req ActionRequest, level int,
) Decision {
	switch {
	case level <= 0:
		return gate.decision(req, VerdictBlocked, ReasonAgentLevel0)
	case level == 1:
		return gate.decision(req, VerdictObserveOnly, ReasonAgentProposalRecorded)
	}
	if req.Contract.RiskTier != RiskReadOnly {
		doc, err := gate.policy(ctx, req)
		if err != nil || ValidateDocument(doc) != nil {
			return blocked(ReasonPolicyUnavailable, errorDetail(err))
		}
		if !containsChangeClass(doc.AllowedChangeClasses, ChangeClass(req.Feature)) {
			return gate.decision(req, VerdictBlocked, ReasonChangeClassNotAllowed)
		}
	}
	return gate.decision(req, VerdictQueueApproval, ReasonApprovalRequired)
}

// agentStop returns the decider's final decision. Only a block or a park
// ends evaluation; anything else from the decider fails closed.
func agentStop(req ActionRequest, decision Decision) Decision {
	if decision.Verdict != VerdictBlocked && decision.Verdict != VerdictPark {
		return blocked(ReasonPolicyUnavailable,
			"agent governance returned a final verdict other than blocked or park")
	}
	return decisionForRequest(req, decision)
}

// agentOperatorCeiling is A5's operator ceiling: observation → L1,
// advisory → L2, autonomous → L3; 0 for an unknown trust level.
func agentOperatorCeiling(trust string) int {
	switch trust {
	case TrustObservation:
		return 1
	case TrustAdvisory:
		return 2
	case TrustAutonomous:
		return 3
	}
	return 0
}

// agentRollbackCap is A5's rollback-class cap: reversible,
// no_rollback_needed and not_applicable allow L3; every other class,
// undeclared included, L2.
func agentRollbackCap(class RollbackClass) int {
	switch class {
	case RollbackReversible, RollbackNoRollbackNeeded, RollbackNotApplicable:
		return 3
	}
	return 2
}

// RequestPrincipal is the agent req comes from: req.Principal, else the
// agent bound to ctx; nil for pg_sage's own requests and people's.
func RequestPrincipal(ctx context.Context, req ActionRequest) *PrincipalRef {
	return withContextPrincipal(ctx, req).Principal
}
