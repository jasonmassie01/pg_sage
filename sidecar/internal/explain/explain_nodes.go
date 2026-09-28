package explain

import (
	"encoding/json"
	"fmt"
	"math"
)

// ---------- node extraction ----------

// extractNodes walks the plan JSON tree and flattens it into a slice.
func extractNodes(planJSON json.RawMessage) []NodeExplain {
	type pgNode struct {
		NodeType     string   `json:"Node Type"`
		RelationName string   `json:"Relation Name"`
		TotalCost    float64  `json:"Total Cost"`
		ActualTime   *float64 `json:"Actual Total Time"`
		PlanRows     int64    `json:"Plan Rows"`
		ActualRows   *float64 `json:"Actual Rows"` // PG18: per-loop average, 2 decimals
		Plans        []pgNode `json:"Plans"`
	}
	type planWrapper struct {
		Plan pgNode `json:"Plan"`
	}
	var wrappers []planWrapper
	if err := json.Unmarshal(planJSON, &wrappers); err != nil || len(wrappers) == 0 {
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
		if n.ActualTime != nil {
			ne.TimeMs = n.ActualTime
		}
		if n.ActualRows != nil {
			ne.Rows = int64(math.Round(*n.ActualRows))
		}
		if n.ActualRows != nil && n.PlanRows > 0 {
			ratio := *n.ActualRows / float64(n.PlanRows)
			if ratio > 10 {
				ne.Warning = fmt.Sprintf(
					"row estimate off by %.0fx (est %d, actual %d)",
					ratio, n.PlanRows, ne.Rows,
				)
			}
		}
		out = append(out, ne)
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(wrappers[0].Plan)
	return out
}

func describeNode(nodeType, relation string) string {
	desc := nodeType
	if relation != "" {
		desc += " on " + relation
	}
	return desc
}
