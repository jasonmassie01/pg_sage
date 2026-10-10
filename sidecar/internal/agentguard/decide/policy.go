package decide

import (
	"context"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Policy is the decider as the standing gate of one database's executor
// sees it (policy.GateConfig.Agents, §6.2.1): A4 of §6.2.2.
func (d *Decider) Policy(database string) policy.AgentDecider {
	return gateAdapter{decider: d, database: database}
}

type gateAdapter struct {
	decider  *Decider
	database string
}

// Decide maps the gate request to a Request, runs the D-steps and maps
// the verdict back: a denial is a final blocked decision, a rate limit a
// final park, and an allowed request continues capped at its level.
func (a gateAdapter) Decide(ctx context.Context, req policy.ActionRequest) (
	policy.Decision, int, bool) {
	if req.Principal == nil {
		return policy.Decision{Verdict: policy.VerdictBlocked,
			Reason: policy.Reason(ReasonUnavailable),
			Detail: "an agent request reached agent governance without its principal"}, 0, true
	}
	v := a.decider.Decide(ctx, toRequest(a.database, req))
	if v.Allowed {
		return policy.Decision{}, v.MaxLevel, false
	}
	verdict := policy.VerdictBlocked
	if v.Park {
		verdict = policy.VerdictPark
	}
	detail := v.Detail
	if v.Fix != "" {
		detail += ". Fix: " + v.Fix
	}
	return policy.Decision{Verdict: verdict, Reason: policy.Reason(v.Reason),
		Detail: detail}, 0, true
}

// toRequest maps a gate request. A read tool's request that reaches the
// gate proposes a change, so it is decided as a proposal, never as a read.
func toRequest(database string, req policy.ActionRequest) Request {
	ref := req.Principal
	kind := KindFor(ref.Tool)
	if kind == agentguard.ToolRead {
		kind = agentguard.ToolPropose
	}
	capability := Capability(req.CapabilityClass)
	if capability == "" {
		capability = CapabilityFor(ref.Tool, req.SQL)
	}
	return Request{PrincipalID: ref.ID, Tool: ref.Tool, Kind: kind, Capability: capability,
		Database: database, Narrowing: policy.IsNarrowing(req),
		OperatorApproved: req.OperatorApproved, TaskID: ref.TaskID}
}
