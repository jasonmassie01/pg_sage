package mcp

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// NeverApproved wraps the policy gate every MCP request goes through. No
// MCP request is a person's approval, an owner's declaration, a rollback
// of pg_sage's own change or the re-authorization under a held lease, so
// those flags are cleared whatever a planner or an argument set: an MCP
// client can propose, and the gate decides as it does for pg_sage's own
// initiative. A missing gate fails closed.
func NeverApproved(gate policy.Gate) policy.Gate { return neverApprovedGate{inner: gate} }

type neverApprovedGate struct{ inner policy.Gate }

func (g neverApprovedGate) Authorize(ctx context.Context,
	request policy.ActionRequest) policy.Decision {
	if g.inner == nil {
		return policy.Decision{Verdict: policy.VerdictBlocked,
			Reason: policy.ReasonPolicyUnavailable, Detail: "MCP policy gate is unavailable"}
	}
	request.OperatorApproved, request.OwnerDeclared = false, false
	request.Rollback, request.LeaseHeld = false, false
	return g.inner.Authorize(ctx, request)
}
