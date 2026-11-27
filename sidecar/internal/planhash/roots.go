package planhash

import (
	"bytes"
	"errors"
	"slices"
)

// ErrUnresolved reports a relation a resolver could not map: it is not in
// the catalog, or its unqualified name exists in more than one schema.
var ErrUnresolved = errors.New("planhash: relation not resolved")

// Relation is a relation (table or index) as EXPLAIN names it. Schema is
// empty in plans captured without VERBOSE.
type Relation struct {
	Schema string
	Name   string
}

// Root is the partition root a relation belongs to. A relation that is
// not a partition is its own root.
type Root struct {
	Schema string
	Name   string
	// Leaves counts the root's leaf partitions; 0 when unknown or when
	// the relation is not partitioned.
	Leaves int
}

// RootResolver maps a relation to its partition root. An error, or a root
// without a name, leaves the relation unresolved.
type RootResolver interface {
	Root(schema, rel string) (Root, error)
}

// Options tunes ComputeWith. The zero value computes an unresolved hash.
type Options struct {
	Roots RootResolver
}

// encoder carries one fingerprint's resolver and whether every relation
// it met was resolved.
type encoder struct {
	roots    RootResolver
	resolved bool
}

// root resolves one relation; ok is false when it stays as EXPLAIN named it.
func (e *encoder) root(schema, rel string) (Root, bool) {
	if e.roots != nil {
		if r, err := e.roots.Root(schema, rel); err == nil && r.Name != "" {
			return r, true
		}
	}
	e.resolved = false
	return Root{Schema: schema, Name: rel}, false
}

// relation resolves the node's "Relation Name", if it has one.
func (e *encoder) relation(node map[string]any) (Root, bool) {
	rel, ok := node["Relation Name"].(string)
	if !ok {
		return Root{}, false
	}
	schema, _ := node["Schema"].(string)
	return e.root(schema, rel)
}

// fields writes the structural fields, with the relation and its index
// named by their partition roots.
func (e *encoder) fields(b *bytes.Buffer, node map[string]any) {
	rel, hasRel := e.relation(node)
	schema, _ := node["Schema"].(string)
	for _, key := range structuralKeys {
		val, ok := scalarString(node[key])
		if !ok {
			continue
		}
		switch {
		case key == "Schema" && hasRel:
			val = rel.Schema
		case key == "Relation Name":
			val = rel.Name
		case key == "Index Name":
			idx, _ := e.root(schema, val)
			val = idx.Name
		}
		writeField(b, key, val)
	}
}

// appendChildren encodes an Append's children as a count bucket, a
// runtime-pruning flag and the sorted set of distinct child shapes, so
// which partitions (and how many of a bucket) a capture scanned does not
// matter, but a lost or gained prune does.
func (e *encoder) appendChildren(b *bytes.Buffer, node map[string]any,
	children []map[string]any, depth int) error {
	shapes := make([]string, 0, len(children))
	for _, child := range children {
		var cb bytes.Buffer
		if err := e.node(&cb, child, depth+1); err != nil {
			return err
		}
		shapes = append(shapes, cb.String())
	}
	writeField(b, "Children", childBucket(len(children), e.sharedLeaves(children)))
	if removed, ok := node["Subplans Removed"].(float64); ok && removed > 0 {
		writeField(b, "Subplans Removed", "true")
	}
	slices.Sort(shapes)
	for _, s := range slices.Compact(shapes) {
		b.WriteString(s)
	}
	return nil
}

// sharedLeaves is the leaf count of the one root every child scans, or 0
// when the children do not all scan resolved partitions of one root.
func (e *encoder) sharedLeaves(children []map[string]any) int {
	var shared Root
	for i, child := range children {
		r, ok := e.relation(child)
		if !ok || r.Leaves <= 0 {
			return 0
		}
		if i > 0 && (r.Schema != shared.Schema || r.Name != shared.Name) {
			return 0
		}
		shared = r
	}
	return shared.Leaves
}

// childBucket buckets an Append's child count. 10 and 11 children are in
// different buckets (the documented edge); "all" needs a known leaf count.
func childBucket(n, leaves int) string {
	switch {
	case leaves > 0 && n == leaves:
		return "all"
	case n == 0:
		return "0"
	case n == 1:
		return "1"
	case n <= 10:
		return "2-10"
	default:
		return ">10"
	}
}

// relations lists the distinct tables and indexes a plan names, in plan
// order, so one catalog read can resolve them.
func relations(root map[string]any) ([]Relation, error) {
	var out []Relation
	seen := map[Relation]bool{}
	add := func(r Relation) {
		if r.Name != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	var walk func(node map[string]any, depth int) error
	walk = func(node map[string]any, depth int) error {
		if depth > MaxDepth {
			return invalid("plan depth exceeds %d", MaxDepth)
		}
		schema, _ := node["Schema"].(string)
		for _, key := range []string{"Relation Name", "Index Name"} {
			if name, ok := node[key].(string); ok {
				add(Relation{Schema: schema, Name: name})
			}
		}
		children, err := childNodes(node)
		for _, child := range children {
			if err == nil {
				err = walk(child, depth+1)
			}
		}
		return err
	}
	if err := walk(root, 1); err != nil {
		return nil, err
	}
	return out, nil
}
