package policy

import "context"

// Agent-originated requests (AGENTDB-SPEC §6.2). policy imports no internal
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
