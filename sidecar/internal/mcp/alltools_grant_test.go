package mcp

import "context"

// agent_request_capability in the all-tools census backend.

func init() {
	allToolsArgs[toolRequestCapability] = `{"capability":"read","objects":["app.t"],` +
		`"duration_minutes":5,"reason":"census"}`
}

func (b *allToolsBackend) RequestCapability(ctx context.Context, _ string,
	_ CapabilityRequest) (CapabilityResult, error) {
	b.hit(ctx, toolRequestCapability)
	return CapabilityResult{Verdict: VerdictBlocked, ReasonCode: "agent_unsponsored"}, nil
}
