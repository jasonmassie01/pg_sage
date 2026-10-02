package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// TargetKind is the catalog kind of a leased object.
type TargetKind string

const (
	TargetTable            TargetKind = "table"
	TargetPartitionedTable TargetKind = "partitioned_table"
	TargetIndex            TargetKind = "index"
	TargetMaterializedView TargetKind = "materialized_view"
	TargetRelation         TargetKind = "relation"
	// TargetUnresolved is a schema-qualified name with no catalog object:
	// only its name can be leased.
	TargetUnresolved TargetKind = "unresolved"
)

// TypedTarget is the exact identity of an object a change lease covers.
type TypedTarget struct {
	Kind   TargetKind
	Schema string
	Name   string
	OID    uint32
}

// Canonical is the target's schema-qualified name in the canonical form
// NormalizeTargetObjects produces (lower-case identifiers bare, others
// quoted).
func (t TypedTarget) Canonical() string {
	return canonicalIdentifier(t.Schema) + "." + canonicalIdentifier(t.Name)
}

// LeaseKeys are the advisory keys a lease on the target takes: its name,
// as every name-based lease does, and its OID, which survives a rename and
// any spelling of the name.
func (t TypedTarget) LeaseKeys() []string {
	keys := []string{t.Canonical()}
	if t.OID > 0 {
		keys = append(keys, oidLeaseKey(t.OID))
	}
	return keys
}

// TypedLeaseKeys are the sorted, distinct lease keys of targets.
func TypedLeaseKeys(targets []TypedTarget) []string {
	seen := map[string]bool{}
	var keys []string
	for _, target := range targets {
		for _, key := range target.LeaseKeys() {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func oidLeaseKey(oid uint32) string {
	return "oid:" + strconv.FormatUint(uint64(oid), 10)
}

func canonicalIdentifier(name string) string {
	if validUnquotedIdentifier(name) && name == strings.ToLower(name) {
		return name
	}
	return `"` + name + `"`
}

// TargetQuerier reads the catalog to resolve targets.
type TargetQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// resolveTargetsSQL resolves each name like the statement naming it would
// (unqualified names through search_path) and adds an index's table.
const resolveTargetsSQL = `/* pg_sage */
SELECT n.ord, COALESCE(c.oid, 0)::oid, COALESCE(c.relkind::text, ''),
       COALESCE(ns.nspname::text, ''), COALESCE(c.relname::text, ''),
       COALESCE(p.oid, 0)::oid, COALESCE(p.relkind::text, ''),
       COALESCE(pns.nspname::text, ''), COALESCE(p.relname::text, '')
FROM unnest($1::text[]) WITH ORDINALITY AS n(input, ord)
LEFT JOIN pg_class c ON c.oid = to_regclass(n.input)
LEFT JOIN pg_namespace ns ON ns.oid = c.relnamespace
LEFT JOIN pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_class p ON p.oid = i.indrelid
LEFT JOIN pg_namespace pns ON pns.oid = p.relnamespace
ORDER BY n.ord`

// ResolveTypedTargets resolves names (as a statement writes them) to the
// objects a lease must cover. An index brings its table: a change to the
// index is a change to the table's access paths. A qualified name with no
// catalog object is kept by name. An unqualified one is an error: no name
// key can be derived for it, and a change must never run unleased.
func ResolveTypedTargets(
	ctx context.Context, db TargetQuerier, names []string,
) ([]TypedTarget, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if db == nil {
		return nil, errors.New("resolve lease targets: database is unavailable")
	}
	inputs, qualified, err := targetInputs(names)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(ctx, resolveTargetsSQL, inputs)
	if err != nil {
		return nil, fmt.Errorf("resolve lease targets: %w", err)
	}
	defer rows.Close()
	var targets []TypedTarget
	for rows.Next() {
		resolved, err := scanResolvedTarget(rows, qualified)
		if err != nil {
			return nil, err
		}
		targets = append(targets, resolved...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve lease targets: %w", err)
	}
	return dedupeTargets(targets), nil
}

// targetInputs validates each name and renders it for to_regclass. A
// qualified name also yields its canonical unresolved target.
func targetInputs(names []string) ([]string, map[int64]TypedTarget, error) {
	inputs := make([]string, 0, len(names))
	qualified := map[int64]TypedTarget{}
	for index, name := range names {
		parts, err := splitQualifiedIdentifier(strings.TrimSpace(name))
		if err != nil || len(parts) < 1 || len(parts) > 2 {
			return nil, nil, fmt.Errorf("lease target %q is not a relation name", name)
		}
		rendered := make([]string, len(parts))
		plain := make([]string, len(parts))
		for i, part := range parts {
			value, _, err := normalizeIdentifier(part)
			if err != nil {
				return nil, nil, fmt.Errorf("lease target %q: %w", name, err)
			}
			plain[i], rendered[i] = value, quoteIdentifier(value)
		}
		inputs = append(inputs, strings.Join(rendered, "."))
		if len(plain) == 2 {
			qualified[int64(index+1)] = TypedTarget{Kind: TargetUnresolved,
				Schema: plain[0], Name: plain[1]}
		}
	}
	return inputs, qualified, nil
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func scanResolvedTarget(rows pgx.Rows, qualified map[int64]TypedTarget) ([]TypedTarget, error) {
	var ord int64
	var oid, parentOID uint32
	var kind, schema, name, parentKind, parentSchema, parentName string
	if err := rows.Scan(&ord, &oid, &kind, &schema, &name, &parentOID, &parentKind,
		&parentSchema, &parentName); err != nil {
		return nil, fmt.Errorf("resolve lease targets: %w", err)
	}
	if oid == 0 {
		if target, ok := qualified[ord]; ok {
			return []TypedTarget{target}, nil
		}
		return nil, fmt.Errorf("lease target %d is unqualified and names no relation "+
			"on the search path", ord)
	}
	targets := []TypedTarget{{Kind: targetKind(kind), Schema: schema, Name: name, OID: oid}}
	if parentOID > 0 {
		targets = append(targets, TypedTarget{Kind: targetKind(parentKind),
			Schema: parentSchema, Name: parentName, OID: parentOID})
	}
	return targets, nil
}

func targetKind(relkind string) TargetKind {
	switch relkind {
	case "r":
		return TargetTable
	case "p":
		return TargetPartitionedTable
	case "i", "I":
		return TargetIndex
	case "m":
		return TargetMaterializedView
	}
	return TargetRelation
}

func dedupeTargets(targets []TypedTarget) []TypedTarget {
	seen := map[string]bool{}
	result := make([]TypedTarget, 0, len(targets))
	for _, target := range targets {
		key := target.Canonical() + "\x00" + strconv.FormatUint(uint64(target.OID), 10)
		if !seen[key] {
			seen[key] = true
			result = append(result, target)
		}
	}
	return result
}
