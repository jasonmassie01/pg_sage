package mcp

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
)

// AgentQuery routes agent_query to agent governance's readapi.
func (backend *ProductionBackend) AgentQuery(ctx context.Context,
	req readapi.Request) (readapi.Result, error) {
	if backend.dependencies.AgentBroker == nil {
		return readapi.Result{}, fmt.Errorf("%w: agent governance is not configured",
			readapi.ErrUnavailable)
	}
	return backend.dependencies.AgentBroker.AgentQuery(ctx, req)
}

// AgentWhoAmI routes agent_whoami to agent governance's readapi.
func (backend *ProductionBackend) AgentWhoAmI(ctx context.Context) (readapi.WhoAmI, error) {
	if backend.dependencies.AgentBroker == nil {
		return readapi.WhoAmI{}, fmt.Errorf("%w: agent governance is not configured",
			readapi.ErrUnavailable)
	}
	return backend.dependencies.AgentBroker.AgentWhoAmI(ctx)
}
