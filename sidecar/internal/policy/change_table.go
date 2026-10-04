package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ResolveChangeTables maps relation names, as statements write them, to
// the canonical name of the table each one changes: a table is itself, an
// index its table (the lease rule of ResolveTypedTargets: a change to an
// index is a change to its table's access paths). A qualified name with no
// catalog object keeps its own canonical name. A name that is not a
// relation name, or an unqualified one the search path does not resolve,
// is left out: it identifies no object.
func ResolveChangeTables(
	ctx context.Context, db TargetQuerier, names []string,
) (map[string]string, error) {
	result := map[string]string{}
	var inputs, kept []string
	for _, name := range names {
		rendered, ok := renderRelationName(name)
		if ok {
			inputs, kept = append(inputs, rendered), append(kept, name)
		}
	}
	if len(inputs) == 0 {
		return result, nil
	}
	if db == nil {
		return nil, errors.New("resolve change tables: database is unavailable")
	}
	rows, err := db.Query(ctx, resolveTargetsSQL, inputs)
	if err != nil {
		return nil, fmt.Errorf("resolve change tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ord int64
		var oid, parentOID uint32
		var kind, schema, name, parentKind, parentSchema, parentName string
		if err := rows.Scan(&ord, &oid, &kind, &schema, &name, &parentOID, &parentKind,
			&parentSchema, &parentName); err != nil {
			return nil, fmt.Errorf("resolve change tables: %w", err)
		}
		if table := changeTable(kept[ord-1], oid, schema, name, parentOID,
			parentSchema, parentName); table != "" {
			result[kept[ord-1]] = table
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve change tables: %w", err)
	}
	return result, nil
}

// changeTable is the canonical table of one resolved row.
func changeTable(input string, oid uint32, schema, name string, parentOID uint32,
	parentSchema, parentName string) string {
	switch {
	case parentOID > 0:
		return TypedTarget{Schema: parentSchema, Name: parentName}.Canonical()
	case oid > 0:
		return TypedTarget{Schema: schema, Name: name}.Canonical()
	}
	parts, err := splitQualifiedIdentifier(strings.TrimSpace(input))
	if err != nil || len(parts) != 2 {
		return ""
	}
	plain := make([]string, 2)
	for i, part := range parts {
		value, _, err := normalizeIdentifier(part)
		if err != nil {
			return ""
		}
		plain[i] = value
	}
	return TypedTarget{Schema: plain[0], Name: plain[1]}.Canonical()
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
