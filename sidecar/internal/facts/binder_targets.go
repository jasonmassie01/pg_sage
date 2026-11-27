package facts

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// TargetResolver finds, for unqualified target names, the user schemas
// that have a relation of that name, whatever the search_path.
type TargetResolver interface {
	TargetSchemas(ctx context.Context, names []string) (map[string][]string, error)
}

// qualifyTargets merges a request's targets into its SQL refs. When the
// resolver is also a TargetResolver, an unqualified target relation
// becomes one qualified ref per user schema that has it, and is dropped
// when none does: "instance", a database name or "llm" name no relation,
// so they never bind. Qualified and slot targets are kept as written, and
// without a TargetResolver every target is. SQL-named refs are never
// dropped: when unsure, a fact binds.
func (b *Binder) qualifyTargets(ctx context.Context, sqlOnly, targets []ObjectRef) (
	[]ObjectRef, error) {
	tr, ok := b.resolver.(TargetResolver)
	names := unqualifiedNames(targets)
	if !ok || len(names) == 0 {
		return mergeRefs(sqlOnly, targets), nil
	}
	schemas, err := tr.TargetSchemas(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("resolve target relations %v: %w", names, err)
	}
	var out []ObjectRef
	for _, t := range targets {
		if !unqualifiedRelation(t) {
			out = append(out, t)
			continue
		}
		for _, s := range schemas[t.Name] {
			out = append(out, ObjectRef{Kind: KindRelation, Schema: s, Name: t.Name})
		}
	}
	return mergeRefs(sqlOnly, out), nil
}

// slotTargets keeps the slot refs of targets that name no relation.
func slotTargets(refs []ObjectRef) []ObjectRef {
	var out []ObjectRef
	for _, r := range refs {
		if r.Kind == KindSlot {
			out = append(out, r)
		}
	}
	return out
}

func unqualifiedRelation(r ObjectRef) bool {
	return r.Kind == KindRelation && r.Schema == ""
}

// unqualifiedNames lists the distinct names of unqualified relation refs.
func unqualifiedNames(refs []ObjectRef) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range refs {
		if unqualifiedRelation(r) && !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r.Name)
		}
	}
	return out
}

// targetSchemasSQL finds every user schema with a relation of each name;
// pg_sage's own and the system schemas are not targets.
const targetSchemasSQL = `/* pg_sage facts */
SELECT c.relname::text, n.nspname::text
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = ANY($1::text[])
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
ORDER BY 1, 2`

// TargetSchemas implements TargetResolver with one bounded, read-only
// catalog read. A name no user schema has is absent from the result.
func (c *CatalogResolver) TargetSchemas(ctx context.Context, names []string) (
	map[string][]string, error) {
	out := map[string][]string{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := catalogread.New(c.pool, catalogread.Default()).Query(ctx,
		targetSchemasSQL, names)
	if err != nil {
		return nil, fmt.Errorf("read target schemas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, schema string
		if err := rows.Scan(&name, &schema); err != nil {
			return nil, fmt.Errorf("scan target schema: %w", err)
		}
		out[name] = append(out[name], schema)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read target schemas: %w", err)
	}
	return out, nil
}
