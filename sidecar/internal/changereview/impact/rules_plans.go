package impact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// planNode is one node of an EXPLAIN (FORMAT JSON) plan.
type planNode struct {
	NodeType       string     `json:"Node Type"`
	RelationName   string     `json:"Relation Name"`
	PlanRows       float64    `json:"Plan Rows"`
	TotalCost      float64    `json:"Total Cost"`
	ParallelAware  bool       `json:"Parallel Aware"`
	WorkersPlanned int        `json:"Workers Planned"`
	Plans          []planNode `json:"Plans"`
}

// planOnly runs a plan-only EXPLAIN of a read statement through
// internal/explain (BEGIN READ ONLY, statement_timeout; never ANALYZE).
func (r *run) planOnly(sql string) (planNode, error) {
	cfg := &config.ExplainConfig{TimeoutMs: int(r.e.d.ExplainTimeout.Milliseconds())}
	ex := explain.New(r.e.d.Pool, cfg, func(level, format string, args ...any) {
		r.e.logf("change review impact: explain %s: "+format, append([]any{level},
			args...)...)
	})
	res, err := ex.Explain(r.ctx, explain.ExplainRequest{Query: sql, PlanOnly: true})
	if err != nil {
		return planNode{}, err
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(res.PlanJSON, &plans); err != nil || len(plans) == 0 {
		return planNode{}, fmt.Errorf("unreadable plan: %v", err)
	}
	return plans[0].Plan, nil
}

// seqScans is every sequential scan of the plan with its estimated rows
// (a parallel scan counts its Gather's rows, not one worker's).
func (n planNode) seqScans(gatherRows float64) []seqScan {
	var out []seqScan
	if strings.HasPrefix(n.NodeType, "Gather") {
		gatherRows = n.PlanRows
	}
	if n.NodeType == "Seq Scan" {
		rows := n.PlanRows
		if n.ParallelAware && gatherRows > 0 {
			rows = gatherRows
		}
		out = append(out, seqScan{Relation: n.RelationName, Rows: rows})
	}
	for _, c := range n.Plans {
		out = append(out, c.seqScans(gatherRows)...)
	}
	return out
}

func (n planNode) summary() string {
	s := fmt.Sprintf("%s (%.0f rows, cost %.0f)", n.NodeType, n.PlanRows, n.TotalCost)
	if n.RelationName != "" {
		s = fmt.Sprintf("%s on %s (%.0f rows, cost %.0f)", n.NodeType, n.RelationName,
			n.PlanRows, n.TotalCost)
	}
	for _, sc := range n.seqScans(0) {
		if sc.Relation != n.RelationName {
			s += fmt.Sprintf("; Seq Scan on %s (%.0f rows)", sc.Relation, sc.Rows)
		}
	}
	return s
}

// planRules is CR-12 for a migration's UPDATE, DELETE or INSERT ...
// SELECT (its read shape) and CR-14 for a read statement.
func (r *run) planRules(s *stmt) {
	switch s.kind {
	case string(sqlast.DDLUpdate), string(sqlast.DDLDelete), string(sqlast.DDLInsert):
		r.backfill(s)
	case "select":
		r.statementPlan(s)
	}
}

// planEvidence plans shape for statement s and records "plan:<n>".
func (r *run) planEvidence(s *stmt, shape string) (planData, string, bool) {
	root, err := r.planOnly(denseParams(shape))
	if err != nil {
		if r.ctx.Err() != nil {
			r.err = r.ctx.Err()
			return planData{}, "", false
		}
		s.planErr = err.Error()
		r.unknownFact(s, "plan of statement "+fmt.Sprint(s.index), planFailure(err),
			err.Error(), "")
		return planData{}, "", false
	}
	d := planData{Statement: s.index, Shape: shape, Summary: root.summary(),
		Rows: root.PlanRows, TotalCost: root.TotalCost}
	for _, sc := range root.seqScans(0) {
		sc.Relation = r.qualifyScan(s, sc.Relation)
		d.SeqScans = append(d.SeqScans, sc)
	}
	s.planSummary = d.Summary
	id := r.addEvidence("plan", fmt.Sprint(s.index), "plan of statement "+
		fmt.Sprint(s.index)+": "+d.Summary, d)
	return d, id, true
}

func planFailure(err error) string {
	if errors.Is(err, explain.ErrExplainInvalidRequest) {
		return "not plannable"
	}
	return "plan failed"
}

// qualifyScan names a scanned relation as the statement's relations do.
func (r *run) qualifyScan(s *stmt, name string) string {
	for _, rel := range s.relations {
		if _, n, ok := splitQualified(rel); ok && n == name {
			return rel
		}
	}
	return name
}

// backfill is CR-12: the rows a data change touches, from the plan-only
// EXPLAIN of its read shape, in one transaction.
func (r *run) backfill(s *stmt) {
	shape, err := sqlast.ReadShape(s.sql)
	if err != nil {
		s.planErr = "no read shape: " + err.Error()
		r.unknownFact(s, "rows changed by statement "+fmt.Sprint(s.index), "no read shape",
			err.Error(), "batch the change by key ranges")
		return
	}
	d, id, ok := r.planEvidence(s, shape)
	if !ok {
		return
	}
	where := "a single transaction"
	if r.transactional(s) {
		where += " (with the rest of its migration file)"
	}
	r.find(RuleBackfill, backfillSeverity(d.Rows, r.cfg), fmt.Sprintf("Changes about %.0f "+
		"rows in one statement", d.Rows), fmt.Sprintf("The plan-only EXPLAIN of its read "+
		"shape estimates %.0f rows (%s). They change in %s, holding row locks and "+
		"writing WAL until commit; batch it by key ranges for large tables.", d.Rows,
		d.Summary, where), s.evID, id)
}

// statementPlan is CR-14: large sequential scans in a read the change ships.
func (r *run) statementPlan(s *stmt) {
	d, id, ok := r.planEvidence(s, s.sql)
	if !ok {
		return
	}
	for _, sc := range d.SeqScans {
		if sc.Rows < float64(r.cfg.LargeScanRows) {
			continue
		}
		r.find(RuleStatementPlan, changereview.SeverityWarning, fmt.Sprintf("Seq Scan of "+
			"%s (%.0f rows)", sc.Relation, sc.Rows), fmt.Sprintf("The plan-only plan of "+
			"statement %d reads %s with a Seq Scan of about %.0f rows (%s); an index on "+
			"its filter avoids it.", s.index, sc.Relation, sc.Rows, d.Summary), s.evID, id)
	}
}
