package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/explain"
)

// ExplainRequest names one read statement, by text or by its
// pg_stat_statements id. Analyze asks for EXPLAIN ANALYZE, which runs only
// when the explain package's guard proves the statement read-only and
// safe; parameterized statements are always planned without running.
type ExplainRequest struct {
	Query   string   `json:"query,omitempty"`
	QueryID QueryID  `json:"query_id,omitempty"`
	Analyze bool     `json:"analyze,omitempty"`
	Params  []string `json:"params,omitempty"`
}

// ExplainResult is the plan and what it says. SlowBecause and
// Recommendations are the explain package's deterministic plan rules (no
// LLM here); PlanSource and PlanAgeS say where the plan came from
// (tuning-10). Verdict and the fields after it are set only for an agent's
// EXPLAIN (ExplainAsAgent): the broker's verdict, and when the plan was
// refused or failed, why.
type ExplainResult struct {
	Query           string                `json:"query"`
	PlanJSON        json.RawMessage       `json:"plan_json"`
	Summary         string                `json:"summary"`
	SlowBecause     []string              `json:"slow_because,omitempty"`
	Recommendations []string              `json:"recommendations,omitempty"`
	PlanSource      string                `json:"plan_source,omitempty"`
	PlanCapture     string                `json:"plan_capture,omitempty"`
	PlanAgeS        *int64                `json:"plan_age_s,omitempty"`
	NodeBreakdown   []explain.NodeExplain `json:"node_breakdown"`
	EstimatedCost   float64               `json:"estimated_cost"`
	ActualTimeMs    *float64              `json:"actual_time_ms,omitempty"`
	Analyzed        bool                  `json:"analyzed"`
	Note            string                `json:"note,omitempty"`
	AnalyzeRefused  string                `json:"analyze_refused,omitempty"`
	Verdict         string                `json:"verdict,omitempty"`
	Status          string                `json:"status,omitempty"`
	ReasonCode      string                `json:"reason_code,omitempty"`
	Detail          string                `json:"detail,omitempty"`
	Fix             string                `json:"fix,omitempty"`
	SQLState        string                `json:"sqlstate,omitempty"`
	Message         string                `json:"message,omitempty"`
	Retryable       bool                  `json:"retryable,omitempty"`
}

// ExplainQuery explains one read statement (never a write, never several
// statements) in a read-only transaction under a statement timeout, as
// pg_sage's own role: the explainer for people. A query_id is resolved by
// the explain package, exactly as POST /api/v1/explain resolves it, and a
// fresh captured plan with actuals is preferred. Agents use ExplainAsAgent.
func (t *Tools) ExplainQuery(ctx context.Context, req ExplainRequest) (ExplainResult, error) {
	if err := t.explainEnabled(); err != nil {
		return ExplainResult{}, err
	}
	if err := t.ready(); err != nil {
		return ExplainResult{}, err
	}
	query, err := explainTarget(req)
	if err != nil {
		return ExplainResult{}, err
	}
	ex := explain.New(t.pool, t.opts.Explain, t.opts.Log).WithPlanMaxAge(t.opts.PlanMaxAgeMinutes)
	res, err := ex.Explain(ctx, explain.ExplainRequest{Query: query,
		QueryID: explain.QueryID(req.QueryID), PlanOnly: !req.Analyze, Params: req.Params})
	if err != nil {
		return ExplainResult{}, t.explainError(err)
	}
	out := ExplainResult{Query: res.Query, PlanJSON: res.PlanJSON,
		SlowBecause: res.SlowBecause, Recommendations: res.Recommendations,
		PlanSource: res.PlanSource, PlanCapture: res.PlanCapture, PlanAgeS: res.PlanAgeS,
		NodeBreakdown: res.NodeBreakdown, EstimatedCost: res.EstimatedCost,
		ActualTimeMs: res.ActualTimeMs, Analyzed: res.ActualTimeMs != nil, Note: res.Note,
		AnalyzeRefused: res.AnalyzeRefused}
	out.Summary = explainSummary(out)
	return out, nil
}

// explainTarget checks that exactly one of query and query_id is given and
// returns the trimmed query text ("" for a query_id).
func explainTarget(req ExplainRequest) (string, error) {
	query := strings.TrimSpace(req.Query)
	switch {
	case req.Query != "" && req.QueryID != 0:
		return "", invalid("give query or query_id, not both")
	case req.QueryID == 0 && query == "":
		return "", invalid("query or query_id is required")
	}
	return query, nil
}

// explainText is the statement to explain: the given text, or the
// pg_stat_statements text of the given id in this database, read by
// explain.ResolveQueryID (the REST endpoint's lookup) in a bounded read.
func (t *Tools) explainText(ctx context.Context, req ExplainRequest) (string, error) {
	query, err := explainTarget(req)
	if err != nil || req.QueryID == 0 {
		return query, err
	}
	text, err := explain.ResolveQueryID(ctx, catalogread.New(t.pool, catalogread.Default()),
		int64(req.QueryID))
	if err != nil {
		return "", t.explainError(err)
	}
	return text, nil
}

// explainError maps the explain package's errors to this package's: an
// unknown id is ErrNotFound, a missing pg_stat_statements ErrUnavailable,
// any other bad request ErrInvalid. Server text that can quote a value is
// withheld from the caller and logged for the operator (security-10).
func (t *Tools) explainError(err error) error {
	err = t.logWithheld("explain", err, explain.SafeError(err))
	switch {
	case errors.Is(err, explain.ErrUnknownQueryID):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, explain.ErrStatementsUnavailable):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	case errors.Is(err, explain.ErrExplainInvalidRequest):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return fmt.Errorf("explain query: %w", err)
}

// logWithheld returns safe, explain.SafeError(err). When safe withholds
// text, the full error is logged at INFO for the operator, as the agent
// broker does.
func (t *Tools) logWithheld(what string, err, safe error) error {
	if safe != nil && safe.Error() != err.Error() && t != nil && t.opts.Log != nil {
		t.opts.Log("INFO", "agenttools: %s failed (text withheld from the caller): %v",
			what, err)
	}
	return safe
}

// explainSummary is the plan in one line: root node, cost and timing.
func explainSummary(r ExplainResult) string {
	root := "plan"
	if len(r.NodeBreakdown) > 0 {
		root = r.NodeBreakdown[0].NodeType
	}
	s := fmt.Sprintf("%s, estimated total cost %.2f", root, r.EstimatedCost)
	if r.ActualTimeMs != nil {
		s += fmt.Sprintf(", actual time %.3f ms", *r.ActualTimeMs)
	} else {
		s += " (not executed)"
	}
	return s
}
