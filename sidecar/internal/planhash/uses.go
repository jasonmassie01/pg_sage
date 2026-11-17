package planhash

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotPlan reports EXPLAIN (FORMAT JSON) output that holds no plan node:
// not JSON, or JSON without a "Plan" object or a "Node Type".
var ErrNotPlan = errors.New("planhash: not an EXPLAIN plan")

// PlanRoots finds the plan nodes of an EXPLAIN (FORMAT JSON) array, an
// auto_explain object ({"Plan": ...}) or a bare node.
func PlanRoots(doc any) []map[string]any {
	var out []map[string]any
	switch v := doc.(type) {
	case []any:
		for _, item := range v {
			out = append(out, PlanRoots(item)...)
		}
	case map[string]any:
		if node, ok := v["Plan"].(map[string]any); ok {
			return []map[string]any{node}
		}
		if _, ok := v["Node Type"]; ok {
			return []map[string]any{v}
		}
	}
	return out
}

// FindIndex reports whether the plan scans the index named name anywhere
// in its tree: a node's "Index Name" key, never text inside a condition or
// a filter. Children are read from "Plans" (InitPlans and SubPlans
// included), at most MaxDepth deep. It returns ErrNotPlan for input that
// is not a plan; an empty name is never found.
func FindIndex(planJSON []byte, name string) (bool, error) {
	var doc any
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		return false, fmt.Errorf("%w: %v", ErrNotPlan, err)
	}
	roots := PlanRoots(doc)
	if len(roots) == 0 {
		return false, ErrNotPlan
	}
	for _, root := range roots {
		found, err := nodeUsesIndex(root, name, 1)
		if found || err != nil {
			return found, err
		}
	}
	return false, nil
}

// UsesIndex is FindIndex without its error: a plan that is not one uses
// no index.
func UsesIndex(planJSON []byte, name string) bool {
	found, err := FindIndex(planJSON, name)
	return err == nil && found
}

func nodeUsesIndex(node map[string]any, name string, depth int) (bool, error) {
	if depth > MaxDepth {
		return false, fmt.Errorf("planhash: plan depth exceeds %d", MaxDepth)
	}
	if got, ok := node["Index Name"].(string); ok && name != "" && got == name {
		return true, nil
	}
	children, _ := node["Plans"].([]any)
	for _, c := range children {
		child, ok := c.(map[string]any)
		if !ok {
			continue
		}
		found, err := nodeUsesIndex(child, name, depth+1)
		if found || err != nil {
			return found, err
		}
	}
	return false, nil
}
