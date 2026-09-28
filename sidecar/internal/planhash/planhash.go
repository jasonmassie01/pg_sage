// Package planhash computes stable fingerprints of PostgreSQL execution
// plans. Two plans have the same fingerprint when they have the same
// shape: the same node types, join and scan strategies, relations and
// indexes, in the same tree positions. Costs, row estimates, timings,
// buffer counters, aliases and condition text (which carries literals)
// are ignored, so repeated captures of one plan agree and a plan flip
// changes the fingerprint (Sage SRE M0, query_store.plan_hash).
package planhash

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Version prefixes every fingerprint so the algorithm can evolve without
// comparing incompatible hashes.
const Version = "v1"

// MaxDepth bounds plan nesting (the root node is depth 1).
const MaxDepth = 256

// ErrInvalidPlan reports input that is not an EXPLAIN (FORMAT JSON) plan.
var ErrInvalidPlan = errors.New("planhash: invalid plan")

// structuralKeys are the plan-node fields that define plan shape, in the
// fixed order they are encoded.
var structuralKeys = []string{
	"Node Type", "Parent Relationship", "Subplan Name", "Join Type",
	"Strategy", "Partial Mode", "Parallel Aware", "Async Capable",
	"Inner Unique", "Scan Direction", "Operation", "Command",
	"Schema", "Relation Name", "Index Name", "CTE Name", "Function Name",
}

// Compute returns the fingerprint of an EXPLAIN (FORMAT JSON) plan. It
// accepts the array EXPLAIN returns, the object auto_explain logs
// (with a "Plan" key), or a bare root node.
func Compute(planJSON []byte) (string, error) {
	root, err := rootNode(planJSON)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := encodeNode(&b, root, 1); err != nil {
		return "", err
	}
	sum := sha256.Sum256(b.Bytes())
	return Version + ":" + hex.EncodeToString(sum[:16]), nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

// rootNode unwraps the supported envelopes to the root plan node.
func rootNode(planJSON []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(planJSON)) == 0 {
		return nil, invalid("empty input")
	}
	var v any
	if err := json.Unmarshal(planJSON, &v); err != nil {
		return nil, invalid("decode: %v", err)
	}
	if arr, ok := v.([]any); ok {
		if len(arr) == 0 {
			return nil, invalid("empty plan array")
		}
		v = arr[0]
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, invalid("plan is not an object")
	}
	if inner, ok := obj["Plan"].(map[string]any); ok {
		obj = inner
	}
	return obj, nil
}

// encodeNode writes a length-prefixed canonical encoding of node and its
// children, so no field value can forge a different tree.
func encodeNode(b *bytes.Buffer, node map[string]any, depth int) error {
	if depth > MaxDepth {
		return invalid("plan depth exceeds %d", MaxDepth)
	}
	nodeType, ok := node["Node Type"].(string)
	if !ok || nodeType == "" {
		return invalid("node without a Node Type at depth %d", depth)
	}
	b.WriteByte('(')
	for _, key := range structuralKeys {
		if val, ok := scalarString(node[key]); ok {
			writeField(b, key, val)
		}
	}
	children, err := childNodes(node)
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := encodeNode(b, child, depth+1); err != nil {
			return err
		}
	}
	b.WriteByte(')')
	return nil
}

func childNodes(node map[string]any) ([]map[string]any, error) {
	raw, present := node["Plans"]
	if !present {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, invalid("Plans is not an array")
	}
	out := make([]map[string]any, 0, len(arr))
	for _, c := range arr {
		child, ok := c.(map[string]any)
		if !ok {
			return nil, invalid("child plan is not an object")
		}
		out = append(out, child)
	}
	return out, nil
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	default:
		return "", false
	}
}

func writeField(b *bytes.Buffer, key, val string) {
	for _, s := range []string{key, val} {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
}
