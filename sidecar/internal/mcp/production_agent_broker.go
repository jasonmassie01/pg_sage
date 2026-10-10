package mcp

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentguard/broker"
)

// AgentQuery routes agent_query to agent governance's broker.
func (backend *ProductionBackend) AgentQuery(ctx context.Context,
	req broker.Request) (broker.Result, error) {
	if backend.dependencies.AgentBroker == nil {
		return broker.Result{}, fmt.Errorf("%w: agent governance is not configured",
			broker.ErrUnavailable)
	}
	return backend.dependencies.AgentBroker.AgentQuery(ctx, req)
}

// AgentWhoAmI routes agent_whoami to agent governance's broker.
func (backend *ProductionBackend) AgentWhoAmI(ctx context.Context) (broker.WhoAmI, error) {
	if backend.dependencies.AgentBroker == nil {
		return broker.WhoAmI{}, fmt.Errorf("%w: agent governance is not configured",
			broker.ErrUnavailable)
	}
	return backend.dependencies.AgentBroker.AgentWhoAmI(ctx)
}
