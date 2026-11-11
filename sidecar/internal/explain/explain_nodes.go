package explain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// ---------- node extraction ----------

// extractNodes walks the plan JSON tree and flattens it into a slice.
// TimeMs is the node's inclusive time over all its loops (Actual Total
// Time x Actual Loops), so an inner nested-loop node shows what it cost.
func extractNodes(planJSON json.RawMessage) []NodeExplain {
	type pgNode struct {
		NodeType     string   `json:"Node Type"`
		RelationName string   `json:"Relation Name"`
		TotalCost    float64  `json:"Total Cost"`
		ActualTime   *float64 `json:"Actual Total Time"`
		ActualLoops  *float64 `json:"Actual Loops"`
		PlanRows     int64    `json:"Plan Rows"`
		ActualRows   *float64 `json:"Actual Rows"` // PG18: per-loop average, 2 decimals
		Plans        []pgNode `json:"Plans"`
	}
	type planWrapper struct {
		Plan pgNode `json:"Plan"`
	}
	var wrappers []planWrapper
	if err := json.Unmarshal(normalizePlan(planJSON), &wrappers); err != nil ||
		len(wrappers) == 0 {
		return nil
	}

	var out []NodeExplain
	var walk func(n pgNode)
	walk = func(n pgNode) {
		ne := NodeExplain{
			NodeType:    n.NodeType,
			Relation:    n.RelationName,
			Description: describeNode(n.NodeType, n.RelationName),
			RowEstimate: n.PlanRows,
		}
		ne.TimeMs, ne.Loops = nodeTime(n.ActualTime, n.ActualLoops)
		if n.ActualRows != nil {
			ne.Rows = int64(math.Round(*n.ActualRows))
		}
		ne.Warning = misestimateWarning(n.PlanRows, n.ActualRows, n.ActualLoops, ne.Rows)
		out = append(out, ne)
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(wrappers[0].Plan)
	return out
}

// nodeTime is the inclusive time over all loops; a node without "Actual
// Loops" counts one loop. Without actuals both are nil.
func nodeTime(perLoop, loops *float64) (*float64, *int64) {
	if perLoop == nil {
		return nil, nil
	}
	n := 1.0
	var count *int64
	if loops != nil {
		n = *loops
		c := int64(math.Round(n))
		count = &c
	}
	total := *perLoop * n
	return &total, count
}

// misestimateFactor: a node warns when actual and estimated rows (both
// per loop) differ by this factor or more, in either direction.
const misestimateFactor = 10

// misestimateWarning compares per-loop actual rows with the estimate, a
// value below 1 on either side counting as 1: 0 rows against an estimate
// of 50 is a 50x overestimate, and PG18's fractional per-loop averages
// (0.50) compare as PG14-17's whole rows do. A node that never ran (0
// loops) or has no actuals does not warn.
func misestimateWarning(est int64, actual, loops *float64, rows int64) string {
	if actual == nil || (loops != nil && *loops == 0) {
		return ""
	}
	a, e := math.Max(*actual, 1), math.Max(float64(est), 1)
	ratio := math.Max(a/e, e/a)
	if ratio < misestimateFactor {
		return ""
	}
	return fmt.Sprintf("row estimate off by %.0fx (est %d, actual %d)", ratio, est, rows)
}

// normalizePlan wraps a bare {"Plan": ...} document (an auto_explain
// capture) in the array EXPLAIN (FORMAT JSON) prints.
func normalizePlan(plan json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(plan)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return plan
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil || doc["Plan"] == nil {
		return plan
	}
	return json.RawMessage("[" + string(trimmed) + "]")
}

func describeNode(nodeType, relation string) string {
	desc := nodeType
	if relation != "" {
		desc += " on " + relation
	}
	return desc
}
