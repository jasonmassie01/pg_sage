package mcp

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// bindAgentRef marks an agent's tool call as agent-originated for the
// policy gate (AGENTDB-SPEC §6.2.6): every ActionRequest built under ctx
// then carries the principal and the tool. The authenticated MCP
// principal is the authority; a ref the transport bound for the same
// principal keeps its sponsor, task and act claim. A person's call
// carries none.
func bindAgentRef(ctx context.Context, tool string) context.Context {
	p, ok := PrincipalFromContext(ctx)
	if !ok || p.Kind != KindAgent {
		return ctx
	}
	ref, bound := policy.PrincipalRefFromContext(ctx)
	if !bound || ref.ID != p.PrincipalID {
		ref = policy.PrincipalRef{ID: p.PrincipalID}
	}
	ref.Tool = tool
	return policy.WithPrincipalRef(ctx, ref)
}

// StdioPrincipalFor is the stdio client's principal: today's local agent
// (read and propose, never approve), bound to principalID when
// mcp.stdio_principal names one.
func StdioPrincipalFor(principalID string) Principal {
	p := stdioPrincipal
	p.Scopes = append([]Scope(nil), stdioPrincipal.Scopes...)
	p.PrincipalID = principalID
	return p
}

// proposingAgent is the agent behind a policy proposal, nil for a person:
// an agent's widening proposal needs two people to ratify (G1-14).
func proposingAgent(ctx context.Context) *policy.PrincipalRef {
	ref, ok := policy.PrincipalRefFromContext(ctx)
	if !ok {
		return nil
	}
	return &ref
}
