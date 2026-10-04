package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Reads through the sources the wiring supplies: investigations (the
// redacted service), the trust ledger view and the workload's heaviest
// statements (query text sanitized: comments and literals removed).

var investigationID = regexp.MustCompile(`^[0-9a-fA-F-]{1,64}$`)

const maxSourceRunes = 3500

func (ss *session) investigationTools() []agentloop.Tool {
	return []agentloop.Tool{
		tool("list_investigations", "List the database's recent investigations with "+
			"their state, subject and root cause.", `"limit":{"type":"integer",`+
			`"minimum":1,"maximum":20}`, nil, ss.listInvestigations),
		tool("get_investigation", "Read one investigation: hypotheses, evidence and its "+
			"conclusion.", `"id":{"type":"string","maxLength":64}`, []string{"id"},
			ss.getInvestigation),
	}
}

func (ss *session) listInvestigations(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		Limit *int `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	limit, err := limitOf(a.Limit, 10, 20)
	if err != nil {
		return agentloop.Output{}, err
	}
	out, err := ss.s.d.Investigations.ListInvestigations(ctx, limit)
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("list investigations: %w", err)
	}
	return ss.cite("investigations:recent", "ok", clip(string(out), maxSourceRunes)), nil
}

func (ss *session) getInvestigation(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if !investigationID.MatchString(a.ID) {
		return agentloop.Output{}, invalidArgs("id must be an investigation id")
	}
	id := "investigation:" + a.ID
	out, err := ss.s.d.Investigations.Investigation(ctx, a.ID)
	if errors.Is(err, ErrNotFound) {
		return ss.cite(id, "not_found", "investigation "+a.ID+" does not exist"), nil
	}
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read investigation %s: %w", a.ID, err)
	}
	return ss.cite(id, "ok", clip(string(out), maxSourceRunes)), nil
}

func (ss *session) trustTool() agentloop.Tool {
	return tool("trust_ledger", "Read the database's trust ledger: the earned autonomy "+
		"level of each action family and class, with its evidence.", "", nil,
		func(ctx context.Context, raw json.RawMessage) (agentloop.Output, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return agentloop.Output{}, err
			}
			out, err := ss.s.d.Trust.Trust(ctx)
			if err != nil {
				return agentloop.Output{}, fmt.Errorf("read the trust ledger: %w", err)
			}
			return ss.cite("trust:"+ss.s.d.Database, "ok", clip(string(out),
				maxSourceRunes)), nil
		})
}

var topOrders = map[string]bool{"total_time": true, "mean_time": true, "calls": true}

func (ss *session) queriesTool() agentloop.Tool {
	return tool("top_queries", "List the workload's heaviest statements (query text "+
		"with literals and comments removed).", `"order_by":{"type":"string","enum":`+
		`["total_time","mean_time","calls"]},"limit":{"type":"integer","minimum":1,`+
		`"maximum":50}`, nil, ss.topQueries)
}

func (ss *session) topQueries(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		OrderBy string `json:"order_by"`
		Limit   *int   `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if a.OrderBy == "" {
		a.OrderBy = "total_time"
	}
	limit, err := limitOf(a.Limit, 10, 50)
	if err != nil {
		return agentloop.Output{}, err
	}
	if !topOrders[a.OrderBy] {
		return agentloop.Output{}, invalidArgs("unknown order %q", a.OrderBy)
	}
	res, err := ss.s.d.Queries.TopQueries(ctx, agenttools.TopQueriesRequest{Limit: limit,
		OrderBy: a.OrderBy})
	if err != nil {
		return agentloop.Output{}, fmt.Errorf("read top queries: %w", err)
	}
	lines := make([]string, 0, len(res.Queries))
	for _, q := range res.Queries {
		lines = append(lines, fmt.Sprintf("queryid %d: %d calls, %.1f ms total, %.2f ms "+
			"mean, %d rows | %s", q.QueryID, q.Calls, q.TotalTimeMs, q.MeanTimeMs, q.Rows,
			clip(oneLine(llm.SanitizeForLLM(q.Query)), 400)))
	}
	return ss.listOutput("queries:"+a.OrderBy, lines, "pg_stat_statements has no "+
		"application statements"), nil
}
