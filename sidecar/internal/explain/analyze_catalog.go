package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// resolution counts the catalog objects a name can resolve to and how
// many of them are unsafe to execute.
type resolution struct{ matches, unsafe int }

// An unqualified name is matched in every schema of the session's
// effective search_path (current_schemas(true) includes pg_catalog), so
// any volatile overload anywhere it could resolve refuses. Aggregates are
// recorded as IMMUTABLE in pg_proc whatever their support functions do,
// so those are checked too.
const functionResolutionSQL = `
SELECT f.schema, f.name, pg_catalog.count(p.oid)::int,
	pg_catalog.count(*) FILTER (WHERE p.unsafe)::int
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS f(schema, name)
LEFT JOIN LATERAL (
	SELECT pr.oid, (pr.provolatile NOT IN ('i', 's') OR EXISTS (
		SELECT 1 FROM pg_catalog.pg_aggregate a
		JOIN pg_catalog.pg_proc s ON s.oid IN (a.aggtransfn::oid, a.aggfinalfn::oid,
			a.aggcombinefn::oid, a.aggserialfn::oid, a.aggdeserialfn::oid,
			a.aggmtransfn::oid, a.aggminvtransfn::oid, a.aggmfinalfn::oid)
		WHERE a.aggfnoid::oid = pr.oid AND s.provolatile = 'v')) AS unsafe
	FROM pg_catalog.pg_proc pr
	JOIN pg_catalog.pg_namespace n ON n.oid = pr.pronamespace
	WHERE pr.proname = f.name AND CASE WHEN f.schema = ''
		THEN n.nspname = ANY (pg_catalog.current_schemas(true))
		ELSE n.nspname = f.schema END
) p ON true
GROUP BY f.schema, f.name`

const operatorResolutionSQL = `
SELECT o.schema, o.name, pg_catalog.count(p.oid)::int,
	pg_catalog.count(*) FILTER (WHERE p.unsafe)::int
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS o(schema, name)
LEFT JOIN LATERAL (
	SELECT op.oid, (fn.oid IS NULL OR fn.provolatile NOT IN ('i', 's')) AS unsafe
	FROM pg_catalog.pg_operator op
	JOIN pg_catalog.pg_namespace n ON n.oid = op.oprnamespace
	LEFT JOIN pg_catalog.pg_proc fn ON fn.oid = op.oprcode::oid
	WHERE op.oprname = o.name AND CASE WHEN o.schema = ''
		THEN n.nspname = ANY (pg_catalog.current_schemas(true))
		ELSE n.nspname = o.schema END
) p ON true
GROUP BY o.schema, o.name`

// A cast target is unsafe when it is a domain (CHECK constraints run on
// coercion), or its input/receive function or any cast into it is
// volatile.
const typeResolutionSQL = `
SELECT t.schema, t.name, (ty.oid IS NOT NULL)::int,
	(ty.oid IS NOT NULL AND (ty.typtype = 'd' OR EXISTS (
		SELECT 1 FROM pg_catalog.pg_proc p
		WHERE p.oid IN (ty.typinput::oid, ty.typreceive::oid) AND p.provolatile = 'v')
	OR EXISTS (
		SELECT 1 FROM pg_catalog.pg_cast c
		JOIN pg_catalog.pg_proc p ON p.oid = c.castfunc
		WHERE c.casttarget = ty.oid AND p.provolatile = 'v')))::int
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS t(schema, name)
LEFT JOIN pg_catalog.pg_type ty ON ty.oid = pg_catalog.to_regtype(
	CASE WHEN t.schema = '' THEN pg_catalog.format('%I', t.name)
	ELSE pg_catalog.format('%I.%I', t.schema, t.name) END)`

const relationResolutionSQL = `
SELECT r.schema, r.name, c.oid, c.relkind::text, c.relrowsecurity
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS r(schema, name)
LEFT JOIN pg_catalog.pg_class c ON c.oid = pg_catalog.to_regclass(
	CASE WHEN r.schema = '' THEN pg_catalog.format('%I', r.name)
	ELSE pg_catalog.format('%I.%I', r.schema, r.name) END)`

func resolveFunctions(
	ctx context.Context, q catalogQuerier, names []sqlast.QualifiedName,
) (map[sqlast.QualifiedName]resolution, error) {
	return resolveNames(ctx, q, "functions", functionResolutionSQL, names)
}

func resolveOperators(
	ctx context.Context, q catalogQuerier, names []sqlast.QualifiedName,
) (map[sqlast.QualifiedName]resolution, error) {
	return resolveNames(ctx, q, "operators", operatorResolutionSQL, names)
}

func resolveTypes(
	ctx context.Context, q catalogQuerier, names []sqlast.QualifiedName,
) (map[sqlast.QualifiedName]resolution, error) {
	return resolveNames(ctx, q, "types", typeResolutionSQL, names)
}

// resolveNames runs one batched resolution query; every row is
// (schema, name, matches, unsafe).
func resolveNames(
	ctx context.Context, q catalogQuerier, kind, query string,
	names []sqlast.QualifiedName,
) (map[sqlast.QualifiedName]resolution, error) {
	out := make(map[sqlast.QualifiedName]resolution, len(names))
	if len(names) == 0 {
		return out, nil
	}
	schemas, objects := splitNames(names)
	rows, err := q.Query(ctx, query, schemas, objects)
	if err != nil {
		return nil, fmt.Errorf("resolve %s %s: %w", kind, joinNames(names), err)
	}
	defer rows.Close()
	for rows.Next() {
		var name sqlast.QualifiedName
		var r resolution
		if err := rows.Scan(&name.Schema, &name.Name, &r.matches, &r.unsafe); err != nil {
			return nil, fmt.Errorf("scan %s resolution: %w", kind, err)
		}
		out[name] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve %s %s: %w", kind, joinNames(names), err)
	}
	return out, nil
}

type relationInfo struct {
	oid  *uint32
	kind *string
	rls  *bool
}

func resolveRelations(
	ctx context.Context, q catalogQuerier, names []sqlast.QualifiedName,
) (map[sqlast.QualifiedName]relationInfo, error) {
	schemas, objects := splitNames(names)
	rows, err := q.Query(ctx, relationResolutionSQL, schemas, objects)
	if err != nil {
		return nil, fmt.Errorf("resolve relations %s: %w", joinNames(names), err)
	}
	defer rows.Close()
	out := make(map[sqlast.QualifiedName]relationInfo, len(names))
	for rows.Next() {
		var name sqlast.QualifiedName
		var info relationInfo
		if err := rows.Scan(&name.Schema, &name.Name, &info.oid, &info.kind,
			&info.rls); err != nil {
			return nil, fmt.Errorf("scan relation resolution: %w", err)
		}
		out[name] = info
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve relations %s: %w", joinNames(names), err)
	}
	return out, nil
}

// checkRelations resolves every FROM relation. Views are expanded through
// pg_get_viewdef and checked like the query itself, so a view that calls a
// volatile function is refused. A name that does not resolve is a CTE
// reference when a CTE has that name, and unknown otherwise.
func (g *analyzeGuard) checkRelations(
	ctx context.Context, q sqlast.ReadQuery, depth int,
) (string, error) {
	if len(q.Relations) == 0 {
		return "", nil
	}
	found, err := resolveRelations(ctx, g.q, q.Relations)
	if err != nil {
		return "", err
	}
	ctes := make(map[string]bool, len(q.CTENames))
	for _, name := range q.CTENames {
		ctes[name] = true
	}
	for _, rel := range q.Relations {
		info := found[rel]
		if info.oid == nil || info.kind == nil {
			if rel.Schema == "" && ctes[rel.Name] {
				continue
			}
			return "reads unknown relation " + rel.String(), nil
		}
		reason, err := g.checkRelation(ctx, rel, info, depth)
		if err != nil || reason != "" {
			return reason, err
		}
	}
	return "", nil
}

func (g *analyzeGuard) checkRelation(
	ctx context.Context, rel sqlast.QualifiedName, info relationInfo, depth int,
) (string, error) {
	if *info.kind != "v" {
		return relationKindRefusal(rel, *info.kind, info.rls != nil && *info.rls), nil
	}
	g.views++
	if depth >= maxViewDepth || g.views > maxViewsChecked {
		return "reads view " + rel.String() + " nested deeper than the guard follows", nil
	}
	var def string
	if err := g.q.QueryRow(ctx, "SELECT pg_catalog.pg_get_viewdef($1::oid)",
		*info.oid).Scan(&def); err != nil {
		return "", fmt.Errorf("read definition of view %s: %w", rel.String(), err)
	}
	reason, err := g.check(ctx, def, depth+1)
	if err != nil || reason == "" {
		return reason, err
	}
	return "reads view " + rel.String() + " which " + reason, nil
}

func splitNames(names []sqlast.QualifiedName) ([]string, []string) {
	schemas := make([]string, len(names))
	objects := make([]string, len(names))
	for i, n := range names {
		schemas[i], objects[i] = n.Schema, n.Name
	}
	return schemas, objects
}

func joinNames(names []sqlast.QualifiedName) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n.String()
	}
	return strings.Join(parts, ", ")
}
