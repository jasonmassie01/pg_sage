package explain

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// A third call the query text does not show: coercing a value to a domain
// evaluates the domain's CHECK constraints in the caller, and a CHECK may
// call any function. PostgreSQL coerces to a domain when a called
// function or operator takes one (directly, as an array element, a
// composite field or a range subtype), and when an explicit cast targets a
// type built from one (a composite with a domain field). Each CHECK of
// every domain reachable that way is checked as a query of its own
// (env-cgo-c). A cast straight to a domain is refused by checkTypes.

// domainChecksSQL lists the CHECK expressions of every domain reachable
// from the argument types of the named functions and operators (same
// matching as functionResolutionSQL and operatorResolutionSQL) and from
// the named cast targets. pg_get_expr renders the domain value as VALUE.
const domainChecksSQL = `
WITH RECURSIVE named(kind, schema, name) AS (
	SELECT * FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[]),
		pg_catalog.unnest($3::text[]))
), seed(kind, schema, name, typ) AS (
	SELECT f.kind, f.schema, f.name, a.typ FROM named f
	JOIN pg_catalog.pg_proc p ON f.kind = 'function' AND p.proname = f.name
	JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
	CROSS JOIN LATERAL pg_catalog.unnest(p.proargtypes::pg_catalog.oid[]) AS a(typ)
	WHERE CASE WHEN f.schema = '' THEN n.nspname = ANY (pg_catalog.current_schemas(true))
		ELSE n.nspname = f.schema END
	UNION
	SELECT o.kind, o.schema, o.name, a.typ FROM named o
	JOIN pg_catalog.pg_operator op ON o.kind = 'operator' AND op.oprname = o.name
	JOIN pg_catalog.pg_namespace n ON n.oid = op.oprnamespace
	CROSS JOIN LATERAL (VALUES (op.oprleft), (op.oprright)) AS a(typ)
	WHERE a.typ <> 0 AND CASE WHEN o.schema = ''
		THEN n.nspname = ANY (pg_catalog.current_schemas(true))
		ELSE n.nspname = o.schema END
	UNION
	SELECT t.kind, t.schema, t.name, pg_catalog.to_regtype(
		CASE WHEN t.schema = '' THEN pg_catalog.format('%I', t.name)
		ELSE pg_catalog.format('%I.%I', t.schema, t.name) END)::pg_catalog.oid
	FROM named t WHERE t.kind = 'type'
), reach(kind, schema, name, typ) AS (
	SELECT * FROM seed WHERE typ IS NOT NULL
	UNION
	SELECT r.kind, r.schema, r.name, x.typ FROM reach r
	JOIN pg_catalog.pg_type t ON t.oid = r.typ
	CROSS JOIN LATERAL (
		SELECT t.typbasetype WHERE t.typtype = 'd'
		UNION ALL SELECT t.typelem WHERE t.typelem <> 0
		UNION ALL SELECT a.atttypid FROM pg_catalog.pg_attribute a
			WHERE t.typtype = 'c' AND a.attrelid = t.typrelid AND a.attnum > 0
			AND NOT a.attisdropped
		UNION ALL SELECT g.rngsubtype FROM pg_catalog.pg_range g WHERE g.rngtypid = t.oid
		UNION ALL SELECT g.rngtypid FROM pg_catalog.pg_range g WHERE g.rngmultitypid = t.oid
	) AS x(typ)
)
SELECT DISTINCT r.kind, r.schema, r.name, c.contypid::pg_catalog.regtype::text,
	pg_catalog.pg_get_expr(c.conbin, 0)
FROM reach r
JOIN pg_catalog.pg_constraint c ON c.contypid = r.typ AND c.contype = 'c'
ORDER BY 1, 2, 3, 4, 5`

// domainCheck is one CHECK expression a query's coercions evaluate.
type domainCheck struct {
	kind   string // function, operator or type
	owner  sqlast.QualifiedName
	domain string
	expr   string
}

func (d domainCheck) about() string {
	verb := map[string]string{"function": "calls", "operator": "uses operator",
		"type": "casts to"}[d.kind]
	return fmt.Sprintf("%s %s, which coerces to domain %s, whose CHECK constraint",
		verb, d.owner.String(), d.domain)
}

// checkDomainChecks checks every CHECK constraint the query's calls,
// operators and casts may evaluate through a domain coercion.
func (g *analyzeGuard) checkDomainChecks(
	ctx context.Context, q sqlast.ReadQuery, depth int,
) (string, error) {
	kinds, schemas, objects := domainSeeds(q)
	if len(kinds) == 0 {
		return "", nil
	}
	checks, err := g.domainChecks(ctx, kinds, schemas, objects)
	if err != nil || len(checks) == 0 {
		return "", err
	}
	if depth >= maxViewDepth {
		return checks[0].about() + "s are nested deeper than the guard follows", nil
	}
	for _, d := range checks {
		reason, err := g.check(ctx, "SELECT "+d.expr, depth+1)
		if err != nil {
			return "", err
		}
		if reason != "" {
			return d.about() + " " + reason, nil
		}
	}
	return "", nil
}

func domainSeeds(q sqlast.ReadQuery) (kinds, schemas, objects []string) {
	add := func(kind string, list []sqlast.QualifiedName) {
		for _, n := range list {
			kinds, schemas, objects = append(kinds, kind), append(schemas, n.Schema),
				append(objects, n.Name)
		}
	}
	add("function", q.Functions)
	add("function", q.AttributeCalls)
	add("operator", q.Operators)
	add("type", q.Types)
	return kinds, schemas, objects
}

func (g *analyzeGuard) domainChecks(
	ctx context.Context, kinds, schemas, objects []string,
) ([]domainCheck, error) {
	rows, err := g.q.Query(ctx, domainChecksSQL, kinds, schemas, objects)
	if err != nil {
		return nil, fmt.Errorf("read domain CHECK constraints: %w", err)
	}
	defer rows.Close()
	var out []domainCheck
	for rows.Next() {
		var d domainCheck
		if err := rows.Scan(&d.kind, &d.owner.Schema, &d.owner.Name, &d.domain,
			&d.expr); err != nil {
			return nil, fmt.Errorf("scan domain CHECK constraints: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read domain CHECK constraints: %w", err)
	}
	return out, nil
}
