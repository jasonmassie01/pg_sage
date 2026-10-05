package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ChangeTable is the table a relation name changes (an index counts as
// its table) and, when that table belongs to a partition tree, the tree's
// root: a partitioned table, its partitions and their indexes share it.
type ChangeTable struct {
	Table string
	Root  string
}

// resolveChangeTablesSQL resolves each name like the statement naming it
// would (unqualified names through search_path), goes from an index to its
// table, and from a table to the root of its partition tree
// (pg_partition_root reads pg_inherits; NULL outside a partition tree).
const resolveChangeTablesSQL = `/* pg_sage */
SELECT n.ord, COALESCE(t.oid, 0)::oid, COALESCE(tns.nspname::text, ''),
       COALESCE(t.relname::text, ''), COALESCE(rns.nspname::text, ''),
       COALESCE(r.relname::text, '')
FROM unnest($1::text[]) WITH ORDINALITY AS n(input, ord)
LEFT JOIN pg_class c ON c.oid = to_regclass(n.input)
LEFT JOIN pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_class t ON t.oid = COALESCE(i.indrelid, c.oid)
LEFT JOIN pg_namespace tns ON tns.oid = t.relnamespace
LEFT JOIN pg_class r ON r.oid = pg_partition_root(t.oid)
LEFT JOIN pg_namespace rns ON rns.oid = r.relnamespace
ORDER BY n.ord`

// ResolveChangeTables maps relation names, as statements write them, to
// the table each one changes (the lease rule of ResolveTypedTargets: a
// change to an index is a change to its table) and its partition-tree
// root. A qualified name with no catalog object keeps its own canonical
// name. A name that is not a relation name, or an unqualified one the
// search path does not resolve, is left out: it identifies no object.
func ResolveChangeTables(
	ctx context.Context, db TargetQuerier, names []string,
) (map[string]ChangeTable, error) {
	result := map[string]ChangeTable{}
	var inputs, kept []string
	for _, name := range names {
		if rendered, ok := renderRelationName(name); ok {
			inputs, kept = append(inputs, rendered), append(kept, name)
		}
	}
	if len(inputs) == 0 {
		return result, nil
	}
	if db == nil {
		return nil, errors.New("resolve change tables: database is unavailable")
	}
	rows, err := db.Query(ctx, resolveChangeTablesSQL, inputs)
	if err != nil {
		return nil, fmt.Errorf("resolve change tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ord int64
		var oid uint32
		var schema, name, rootSchema, rootName string
		if err := rows.Scan(&ord, &oid, &schema, &name, &rootSchema, &rootName); err != nil {
			return nil, fmt.Errorf("resolve change tables: %w", err)
		}
		if table, ok := changeTable(kept[ord-1], oid, schema, name, rootSchema,
			rootName); ok {
			result[kept[ord-1]] = table
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve change tables: %w", err)
	}
	return result, nil
}

// changeTable is the change table of one resolved row; an unresolved
// qualified name keeps its canonical name and has no root.
func changeTable(input string, oid uint32, schema, name, rootSchema, rootName string) (
	ChangeTable, bool) {
	if oid > 0 {
		table := ChangeTable{Table: TypedTarget{Schema: schema, Name: name}.Canonical()}
		if rootName != "" {
			table.Root = TypedTarget{Schema: rootSchema, Name: rootName}.Canonical()
		}
		return table, true
	}
	parts, err := splitQualifiedIdentifier(strings.TrimSpace(input))
	if err != nil || len(parts) != 2 {
		return ChangeTable{}, false
	}
	plain := make([]string, 2)
	for i, part := range parts {
		value, _, err := normalizeIdentifier(part)
		if err != nil {
			return ChangeTable{}, false
		}
		plain[i] = value
	}
	return ChangeTable{Table: TypedTarget{Schema: plain[0], Name: plain[1]}.Canonical()}, true
}

// renderRelationName renders one relation name for to_regclass, or false
// when it is not one.
func renderRelationName(name string) (string, bool) {
	parts, err := splitQualifiedIdentifier(strings.TrimSpace(name))
	if err != nil || len(parts) < 1 || len(parts) > 2 {
		return "", false
	}
	rendered := make([]string, len(parts))
	for i, part := range parts {
		value, _, err := normalizeIdentifier(part)
		if err != nil || value == "" {
			return "", false
		}
		rendered[i] = quoteIdentifier(value)
	}
	return strings.Join(rendered, "."), true
}
