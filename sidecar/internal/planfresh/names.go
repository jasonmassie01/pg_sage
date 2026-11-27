package planfresh

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Names are the objects an EXPLAIN (FORMAT JSON) plan names.
type Names struct {
	// Qualified are "schema.relation" for nodes that name their schema.
	Qualified []string
	// Unqualified are relations whose node names no schema (plain EXPLAIN).
	Unqualified []string
	// Objects are every relation and index name, unqualified and lower
	// case, for matching pg_sage's action targets.
	Objects []string
}

// Empty reports a plan that names no relation.
func (n Names) Empty() bool { return len(n.Qualified) == 0 && len(n.Unqualified) == 0 }

var (
	errEmptyPlan = errors.New("planfresh: empty plan")
	errNoPlan    = errors.New("planfresh: the document holds no plan node")
)

// PlanNames reads the relation and index names anywhere in the plan
// ([{"Plan":...}], {"Plan":...} or an auto_explain document).
func PlanNames(planJSON []byte) (Names, error) {
	if len(strings.TrimSpace(string(planJSON))) == 0 {
		return Names{}, errEmptyPlan
	}
	var doc any
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		return Names{}, fmt.Errorf("planfresh: parse plan: %w", err)
	}
	q, u, o := map[string]bool{}, map[string]bool{}, map[string]bool{}
	nodes := 0
	walk(doc, func(node map[string]any) {
		if _, ok := node["Node Type"]; ok {
			nodes++
		}
		rel, _ := node["Relation Name"].(string)
		schema, _ := node["Schema"].(string)
		switch {
		case rel != "" && schema != "":
			q[schema+"."+rel] = true
		case rel != "":
			u[rel] = true
		}
		if rel != "" {
			o[strings.ToLower(rel)] = true
		}
		if idx, _ := node["Index Name"].(string); idx != "" {
			o[strings.ToLower(idx)] = true
		}
	})
	if nodes == 0 {
		return Names{}, errNoPlan
	}
	return Names{Qualified: sorted(q), Unqualified: sorted(u), Objects: sorted(o)}, nil
}

func walk(v any, visit func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		visit(t)
		for _, child := range t {
			walk(child, visit)
		}
	case []any:
		for _, child := range t {
			walk(child, visit)
		}
	}
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
