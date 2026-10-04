package agenttools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Bounds of a what-if request.
const (
	maxWhatIfQueries = 20
	maxDDLBytes      = 10000
)

// WhatIfRequest is one candidate index and the statements it should help.
type WhatIfRequest struct {
	DDL      string    `json:"ddl"`
	QueryIDs []QueryID `json:"query_ids"`
}

// WhatIfResult is HypoPG's estimate, judged with the optimizer's bar.
type WhatIfResult struct {
	Available      bool      `json:"available"`
	Verdict        string    `json:"verdict,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	ImprovementPct float64   `json:"improvement_pct"`
	SizeBytes      int64     `json:"size_bytes"`
	Measured       int       `json:"measured"`
	Failed         int       `json:"failed"`
	QueryIDs       []QueryID `json:"query_ids"`
	Note           string    `json:"note,omitempty"`
}

var createIndexPattern = regexp.MustCompile(`(?is)^CREATE\s+(UNIQUE\s+)?INDEX\b`)

// WhatIfIndex plans the statements with and without a hypothetical index
// (HypoPG, session-local: nothing is built). Without HypoPG the result
// says so instead of failing.
func (t *Tools) WhatIfIndex(ctx context.Context, req WhatIfRequest) (WhatIfResult, error) {
	if err := t.ready(); err != nil {
		return WhatIfResult{}, err
	}
	ddl, err := indexDDL(req.DDL)
	if err != nil {
		return WhatIfResult{}, err
	}
	if len(req.QueryIDs) == 0 || len(req.QueryIDs) > maxWhatIfQueries {
		return WhatIfResult{}, invalid("name 1..%d query_ids", maxWhatIfQueries)
	}
	res := WhatIfResult{QueryIDs: req.QueryIDs}
	hypo := optimizer.NewHypoPG(t.pool, t.opts.Log)
	if !hypo.IsAvailable(ctx) {
		res.Note = "HypoPG is not installed in this database, so no hypothetical index " +
			"was planned; install the hypopg extension to compare plans."
		return res, nil
	}
	queries, err := t.whatIfQueries(ctx, req.QueryIDs)
	if err != nil {
		return WhatIfResult{}, err
	}
	measured, err := hypo.Validate(ctx, optimizer.Recommendation{DDL: ddl}, queries)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return WhatIfResult{}, fmt.Errorf("what-if index: %w", ctxErr)
	}
	if err != nil {
		t.opts.Log("WARN", "agenttools: what-if evaluation of %q failed: %v", ddl, err)
	}
	res.Available = true
	res.Verdict, res.Reason = optimizer.WhatIfVerdict(measured, err,
		config.DefaultOptHypoPGMinImprovePct)
	res.ImprovementPct, res.SizeBytes = measured.Improvement, measured.SizeBytes
	res.Measured, res.Failed = measured.Measured, measured.Failed
	res.Note = "Planner cost estimates with a session-local hypothetical index; " +
		"no index was built."
	return res, nil
}

// indexDDL accepts exactly one CREATE [UNIQUE] INDEX statement and returns
// it without a trailing terminator.
func indexDDL(raw string) (string, error) {
	ddl := strings.TrimSpace(raw)
	if ddl == "" {
		return "", invalid("ddl is required")
	}
	if len(ddl) > maxDDLBytes {
		return "", invalid("ddl is longer than %d bytes", maxDDLBytes)
	}
	scan, err := scanSQL(ddl)
	if err != nil {
		return "", invalid("ddl: %v", err)
	}
	if scan.statements != 1 {
		return "", invalid("ddl must be one statement, got %d", scan.statements)
	}
	ddl = strings.TrimSpace(strings.TrimRight(ddl, "; \t\r\n"))
	if !createIndexPattern.MatchString(ddl) {
		return "", invalid("ddl must be one CREATE [UNIQUE] INDEX statement")
	}
	return ddl, nil
}

// whatIfQueries reads the named statements; every id must exist.
func (t *Tools) whatIfQueries(ctx context.Context, ids []QueryID,
) ([]optimizer.QueryInfo, error) {
	found, err := t.statementsByID(ctx, toInt64s(ids))
	if err != nil {
		return nil, err
	}
	out := make([]optimizer.QueryInfo, 0, len(ids))
	var missing []int64
	for _, id := range ids {
		s, ok := found[int64(id)]
		if !ok {
			missing = append(missing, int64(id))
			continue
		}
		out = append(out, optimizer.QueryInfo{QueryID: s.ID, Text: s.Text, Calls: s.Calls,
			MeanTimeMs: s.meanMs(), TotalTimeMs: s.TotalTimeMs})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: no pg_stat_statements entry for %v in this database",
			ErrNotFound, missing)
	}
	return out, nil
}
