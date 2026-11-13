package agenttools

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/explain"
)

// Agent EXPLAIN (security-02). EXPLAIN run as pg_sage's role would give an
// agent row estimates and object existence for tables its own role cannot
// read, and ANALYZE would give actual row counts. So an agent's
// explain_query is planned by a planner that runs as the agent's own
// database role (agent governance's broker), and is never analyzed.

// AgentAnalyzeRefused is why an agent's analyze is not run.
const AgentAnalyzeRefused = "agent principals get plan-only EXPLAIN under their own role"

// agentExplainNote says where an agent's plan came from.
const agentExplainNote = "plan-only EXPLAIN as this agent's own database role"

// AgentPlanner runs one plan-only EXPLAIN as the calling agent's own
// database role. Refusals are results with a verdict; errors are the
// readapi errors.
type AgentPlanner func(ctx context.Context, req readapi.ExplainRequest) (
	readapi.ExplainResult, error)

// explainEnabled refuses every EXPLAIN when explain.enabled is false; the
// setting is read on each call, so a config reload applies at once.
func (t *Tools) explainEnabled() error {
	if t != nil && t.opts.Explain != nil && !t.opts.Explain.Enabled {
		return fmt.Errorf("%w: EXPLAIN is disabled (explain.enabled is false)",
			ErrUnavailable)
	}
	return nil
}

// ExplainAsAgent explains one read statement for an agent: the statement
// (text, or a pg_stat_statements id resolved here) is planned by plan,
// never on pg_sage's pool, and analyze is answered with the plan only.
func (t *Tools) ExplainAsAgent(ctx context.Context, req ExplainRequest,
	plan AgentPlanner) (ExplainResult, error) {
	if err := t.explainEnabled(); err != nil {
		return ExplainResult{}, err
	}
	if err := t.ready(); err != nil {
		return ExplainResult{}, err
	}
	if plan == nil {
		return ExplainResult{}, fmt.Errorf("%w: no planner runs as the agent's own role",
			ErrUnavailable)
	}
	text, err := t.explainText(ctx, req)
	if err != nil {
		return ExplainResult{}, err
	}
	query, err := explain.ReadStatement(text)
	if err != nil {
		return ExplainResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	planned, err := plan(ctx, readapi.ExplainRequest{SQL: query, Params: req.Params})
	if err != nil {
		return ExplainResult{}, t.plannerError(err)
	}
	out := agentExplainResult(query, planned)
	if req.Analyze {
		out.AnalyzeRefused = AgentAnalyzeRefused
	}
	return out, nil
}

// agentExplainResult reads the planner's answer: the plan's breakdown, or
// the verdict that refused it (a normal result the agent can act on).
func agentExplainResult(query string, p readapi.ExplainResult) ExplainResult {
	out := ExplainResult{Query: query, Verdict: p.Verdict, Status: p.Status,
		ReasonCode: p.ReasonCode, Detail: p.Detail, Fix: p.Fix, SQLState: p.SQLState,
		Message: p.Message, Retryable: p.Retryable, Note: agentExplainNote}
	if p.Verdict != readapi.VerdictExecute || p.Status != readapi.StatusOK {
		out.Summary = "not planned: " + refusalText(p)
		return out
	}
	res := explain.EstimatedPlan(query, p.PlanJSON)
	out.PlanJSON, out.NodeBreakdown, out.EstimatedCost = res.PlanJSON, res.NodeBreakdown,
		res.EstimatedCost
	out.Summary = explainSummary(out)
	return out
}

func refusalText(p readapi.ExplainResult) string {
	switch {
	case p.ReasonCode != "":
		return p.ReasonCode
	case p.SQLState != "":
		return "SQLSTATE " + p.SQLState
	}
	return "no plan"
}

// plannerError maps the planner's readapi errors to this package's;
// server text that can quote a value is withheld (security-10).
func (t *Tools) plannerError(err error) error {
	err = t.logWithheld("agent explain", err, explain.SafeError(err))
	switch {
	case errors.Is(err, readapi.ErrInvalid):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case errors.Is(err, readapi.ErrNotPermitted):
		return fmt.Errorf("%w: database not permitted for this principal", ErrInvalid)
	case errors.Is(err, readapi.ErrUnknownDatabase):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, readapi.ErrUnavailable):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return fmt.Errorf("agent explain: %w", err)
}
