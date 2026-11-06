package mcp

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/agenttools"
)

// Agent explain_query (security-02). On pg_sage's own pool an agent's
// EXPLAIN would describe tables the agent's role cannot read, and ANALYZE
// would count their rows. So an agent principal's explain_query is planned
// by agent governance's broker as the agent's own database role, the path
// agent_query takes, and is never analyzed. Without that path it fails
// closed; it never falls back to pg_sage's role.

// AgentExplainBackend plans an agent's EXPLAIN as the agent's own role
// (agent governance's broker).
type AgentExplainBackend interface {
	AgentExplain(ctx context.Context, req readapi.ExplainRequest) (readapi.ExplainResult,
		error)
}

// AgentExplainTools are the coding-agent tools' agent EXPLAIN: the
// statement is resolved on the bound database and planned by plan.
type AgentExplainTools interface {
	ExplainAsAgent(ctx context.Context, req agenttools.ExplainRequest,
		plan agenttools.AgentPlanner) (agenttools.ExplainResult, error)
}

// agentExplain serves an agent principal's explain_query.
func (backend *ProductionBackend) agentExplain(ctx context.Context, tools AgentToolBackend,
	request agenttools.ExplainRequest) (agenttools.ExplainResult, error) {
	explainer, ok := tools.(AgentExplainTools)
	broker, brokered := backend.dependencies.AgentBroker.(AgentExplainBackend)
	if !ok || !brokered {
		return agenttools.ExplainResult{}, fmt.Errorf("%w: an agent's EXPLAIN runs as the "+
			"agent's own role through agent governance, which is not configured (set "+
			"agents.control_database or use mode: meta)", agenttools.ErrUnavailable)
	}
	database, bound := DatabaseFromContext(ctx)
	if !bound || database == "" {
		return agenttools.ExplainResult{}, fmt.Errorf("%w: no database resolved for the "+
			"request", agenttools.ErrInvalid)
	}
	plan := func(ctx context.Context, req readapi.ExplainRequest) (readapi.ExplainResult,
		error) {
		req.Database = database
		return broker.AgentExplain(ctx, req)
	}
	return explainer.ExplainAsAgent(ctx, request, plan)
}
