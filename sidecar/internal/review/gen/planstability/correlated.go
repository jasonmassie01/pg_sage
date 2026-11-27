package planstability

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// estimate is a stored plan's row estimate for a table against what ran.
type estimate struct {
	est, act float64
	drift    bool // no actual rows: the rows each call returned instead
}

func (e estimate) ratio() float64 {
	lo, hi := math.Min(e.est, e.act), math.Max(e.est, e.act)
	return hi / math.Max(lo, 1)
}

// correlated flags two or more equality columns of one table whose stored
// plan misestimates the scan by more than EstimateErrorCliff, and proposes
// pg_sage's extended statistics (dependencies, mcv) on them.
func (j *judge) correlated(q parsedQuery, table string, cols []string) {
	plan, ok := j.in.plans[q.q.QueryID]
	if !ok || plan.Plan != "ok" || plan.Conditions == nil {
		j.cov.Note("query %s: no captured plan with its row counts, so the "+
			"correlated-predicate rule for %s is not checked", q.q.QueryID, table)
		return
	}
	e, ok := planEstimate(plan.Conditions, table, q)
	if !ok || e.ratio() <= EstimateErrorCliff {
		return
	}
	sorted := append([]string(nil), cols...)
	sort.Strings(sorted)
	if len(sorted) > 8 {
		sorted = sorted[:8]
	}
	c, err := statisticsCandidate(table, sorted, j.facts.Tables[table].Owned)
	if err != nil {
		j.cov.Note("query %s: extended statistics on %s skipped: %v", q.q.QueryID, table, err)
		return
	}
	how := "estimated against the actual rows the plan recorded"
	if e.drift {
		how = "estimate drift only: no executed plan, so the estimate is compared with " +
			"the rows each call returned"
	}
	c.Reason = fmt.Sprintf("Top query %s filters %s on %s with equality; the stored plan "+
		"estimated %.0f rows where %.0f came back (%.0fx, %s). Extended statistics let the "+
		"planner see how the columns depend on each other.", q.q.QueryID, table,
		strings.Join(sorted, ", "), e.est, e.act, e.ratio(), how)
	c.EvidenceIDs = []string{plan.ID, q.q.ID}
	sort.Strings(c.EvidenceIDs)
	c.Prediction.Quantities = map[string]float64{"estimate_error": e.ratio(),
		"estimated_rows": e.est, "actual_rows": e.act}
	j.stx = append(j.stx, c)
}

// planEstimate is the scan of table in the plan: its estimate and actual
// rows per loop, or, without actual rows on a one-table query, the root
// estimate against the rows each call returned.
func planEstimate(conditions any, table string, q parsedQuery) (estimate, bool) {
	schema, name, _ := strings.Cut(table, ".")
	var root map[string]any
	if list, ok := conditions.([]any); ok && len(list) > 0 {
		if top, ok := list[0].(map[string]any); ok {
			root, _ = top["Plan"].(map[string]any)
		}
	}
	if root == nil {
		return estimate{}, false
	}
	if node := findScan(root, schema, name); node != nil {
		est, okE := node["Plan Rows"].(float64)
		act, okA := node["Actual Rows"].(float64)
		if okE && okA {
			return estimate{est: est, act: act}, true
		}
	}
	est, ok := root["Plan Rows"].(float64)
	if !ok || len(q.rels) != 1 || q.q.Calls <= 0 {
		return estimate{}, false
	}
	return estimate{est: est, act: float64(q.q.Rows) / float64(q.q.Calls), drift: true}, true
}

// findScan is the first plan node scanning schema.name, depth first.
func findScan(node map[string]any, schema, name string) map[string]any {
	if rel, _ := node["Relation Name"].(string); rel == name {
		if s, ok := node["Schema"].(string); !ok || s == schema {
			return node
		}
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if m, ok := child.(map[string]any); ok {
			if found := findScan(m, schema, name); found != nil {
				return found
			}
		}
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^a-z0-9_]+`)

// statisticsCandidate is pg_sage's CREATE STATISTICS on cols of table, in
// the one form extstats accepts, or an operator step without ownership.
func statisticsCandidate(table string, cols []string, owned bool) (candidate.Candidate,
	error) {
	schema, name, _ := strings.Cut(table, ".")
	sum := sha256.Sum256([]byte(strings.Join(cols, ",")))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	base := unsafeName.ReplaceAllString(strings.ToLower(name), "_")
	if room := 63 - len(extstats.NamePrefix) - len(suffix); len(base) > room {
		base = base[:room]
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = sqlident.Ident(c)
	}
	stx := extstats.NamePrefix + base + suffix
	sql := "CREATE STATISTICS " + sqlident.Qualified(schema, stx) + " (dependencies, mcv) ON " +
		strings.Join(quoted, ", ") + " FROM " + sqlident.Qualified(schema, name)
	parsed, err := extstats.ParseCreate(sql)
	if err != nil {
		return candidate.Candidate{}, err
	}
	c := candidate.Candidate{Generator: Name, Dimension: Dimension, Kind: KindCorrelated,
		Target: table + "(" + strings.Join(cols, ",") + ")", ActionType: "create_statistics",
		ForwardSQL: sql, RollbackSQL: parsed.Rollback(), Reversible: true,
		Approval: candidate.ApprovalAutomatic,
		Prediction: candidate.Prediction{Metric: metricEstimate, Direction: "down",
			Tier: candidate.TierNone, Source: "plans.json stored plan"}}
	if !owned {
		c.ActionType, c.ForwardSQL, c.RollbackSQL = candidate.ActionOperator, "", ""
		c.Approval = candidate.ApprovalRequired
		c.Operator = &candidate.Operator{Command: sql + "; " + parsed.Analyze(),
			Revert: parsed.Rollback(), Setting: "pg_statistic_ext.stxname", Value: stx}
	}
	c.ID = candidate.NewID(Name, c.Kind, c.Target, c.IDBody())
	return c, nil
}
