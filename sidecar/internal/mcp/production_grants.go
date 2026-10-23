package mcp

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// RequestCapability serves agent_request_capability through
// ProductionDependencies.Grants; without it agent grants are unavailable.
func (backend *ProductionBackend) RequestCapability(ctx context.Context, principalID string,
	in CapabilityRequest) (CapabilityResult, error) {
	if backend.dependencies.Grants == nil {
		return CapabilityResult{}, fmt.Errorf("%w: agent grants are not configured",
			agentguard.ErrUnavailable)
	}
	return backend.dependencies.Grants.RequestCapability(ctx, principalID, in)
}
