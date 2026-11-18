package planhash

import (
	"bytes"
	"encoding/json"
)

// shapeNumbers are the numeric plan-node fields Shape keeps beside the
// structural ones: the planner's row and cost estimates and, for an
// EXPLAIN ANALYZE plan, what it measured.
var shapeNumbers = []string{"Plan Rows", "Total Cost", "Actual Rows", "Actual Loops"}

// Shape returns planJSON as shape only (evid-02b): every node keeps its
// structuralKeys (the fields Compute hashes) and the numeric Plan Rows,
// Total Cost, Actual Rows and Actual Loops, with its children under
// "Plans"; every condition, filter, output list, alias and other field is
// dropped, so no literal of the statement survives. It accepts the same
// envelopes as Compute and returns ErrInvalidPlan for anything that is not
// an EXPLAIN (FORMAT JSON) plan. The output is JSON with sorted keys and
// the numbers exactly as written; Compute's fingerprints are unaffected.
func Shape(planJSON []byte) ([]byte, error) {
	root, err := rootNode(planJSON)
	if err != nil {
		return nil, err
	}
	tree, err := shapeNode(root, 1)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, invalid("encode the shape: %v", err)
	}
	return out, nil
}

func shapeNode(node map[string]any, depth int) (map[string]any, error) {
	if depth > MaxDepth {
		return nil, invalid("plan depth exceeds %d", MaxDepth)
	}
	if nodeType, ok := node["Node Type"].(string); !ok || nodeType == "" {
		return nil, invalid("node without a Node Type at depth %d", depth)
	}
	out := map[string]any{}
	for _, k := range structuralKeys {
		if v, ok := node[k]; ok {
			if _, scalar := scalarString(v); scalar {
				out[k] = v
			}
		}
	}
	for _, k := range shapeNumbers {
		if n, ok := number(node[k]); ok {
			out[k] = n
		}
	}
	children, err := childNodes(node)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 {
		kids := make([]any, 0, len(children))
		for _, c := range children {
			kid, err := shapeNode(c, depth+1)
			if err != nil {
				return nil, err
			}
			kids = append(kids, kid)
		}
		out["Plans"] = kids
	}
	return out, nil
}

// number is v as an exact JSON number, when it is one.
func number(v any) (json.Number, bool) {
	f, ok := v.(float64)
	if !ok {
		return "", false
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return "", false
	}
	return json.Number(bytes.TrimSpace(raw)), true
}
