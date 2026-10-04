package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// ExplainResult is the plan and what it says.
type ExplainResult struct {
	Query          string                `json:"query"`
	PlanJSON       json.RawMessage       `json:"plan_json"`
	Summary        string                `json:"summary"`
	NodeBreakdown  []explain.NodeExplain `json:"node_breakdown"`
	EstimatedCost  float64               `json:"estimated_cost"`
	ActualTimeMs   *float64              `json:"actual_time_ms,omitempty"`
	Analyzed       bool                  `json:"analyzed"`
	Note           string                `json:"note,omitempty"`
	AnalyzeRefused string                `json:"analyze_refused,omitempty"`
}

// ExplainQuery explains one read statement (never a write, never several
// statements) in a read-only transaction under a statement timeout.
func (t *Tools) ExplainQuery(ctx context.Context, req ExplainRequest) (ExplainResult, error) {
	if err := t.ready(); err != nil {
		return ExplainResult{}, err
	}
	query, err := t.explainText(ctx, req)
	if err != nil {
		return ExplainResult{}, err
	}
	ex := explain.New(t.pool, t.opts.Explain, t.opts.Log)
	res, err := ex.Explain(ctx, explain.ExplainRequest{Query: query, PlanOnly: !req.Analyze,
		Params: req.Params})
	if errors.Is(err, explain.ErrExplainInvalidRequest) {
		return ExplainResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err != nil {
		return ExplainResult{}, fmt.Errorf("explain query: %w", err)
	}
	out := ExplainResult{Query: res.Query, PlanJSON: res.PlanJSON,
		NodeBreakdown: res.NodeBreakdown, EstimatedCost: res.EstimatedCost,
		ActualTimeMs: res.ActualTimeMs, Analyzed: res.ActualTimeMs != nil, Note: res.Note,
		AnalyzeRefused: res.AnalyzeRefused}
	out.Summary = explainSummary(out)
	return out, nil
}

// explainText is the statement to explain: the given text, or the
// pg_stat_statements text of the given id in this database.
func (t *Tools) explainText(ctx context.Context, req ExplainRequest) (string, error) {
	query := strings.TrimSpace(req.Query)
	switch {
	case req.Query != "" && req.QueryID != 0:
		return "", invalid("give query or query_id, not both")
	case req.QueryID != 0:
		s, err := t.statementByID(ctx, req.QueryID)
		if err != nil {
			return "", err
		}
		return s.Text, nil
	case query == "":
		return "", invalid("query or query_id is required")
	}
	return query, nil
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
