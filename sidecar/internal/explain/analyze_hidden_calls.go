package explain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Calls a query makes that its text does not show: an implicit cast that
// function or operator resolution applies, and the default expressions
// PostgreSQL inserts for omitted arguments. Both execute under EXPLAIN
// ANALYZE and in a brokered read like any call the query names, so a
// volatile function reached this way refuses (env-cgo-c).

// volatileHiddenCastSQL finds a cast PostgreSQL applies without an
// explicit cast in the query (implicit, or assignment) whose function is
// volatile. A stock catalog has none, so the database-wide refusal costs
// nothing there; an explicit cast is judged by checkTypes.
const volatileHiddenCastSQL = `
SELECT c.castsource::pg_catalog.regtype::text, c.casttarget::pg_catalog.regtype::text,
	c.castfunc::pg_catalog.regprocedure::text,
	CASE c.castcontext WHEN 'i' THEN 'implicit' ELSE 'assignment' END
FROM pg_catalog.pg_cast c
JOIN pg_catalog.pg_proc p ON p.oid = c.castfunc
WHERE c.castcontext IN ('i', 'a') AND p.provolatile = 'v'
ORDER BY c.oid LIMIT 1`

// checkImplicitCasts refuses while any volatile implicit or assignment
// cast exists: the parser cannot tell which casts resolution will apply.
// A query that names no function, operator, type or relation has only
// literals, whose coercions are between built-in types, so it is skipped.
func (g *analyzeGuard) checkImplicitCasts(
	ctx context.Context, q sqlast.ReadQuery,
) (string, error) {
	if len(q.Functions)+len(q.AttributeCalls)+len(q.Operators)+len(q.Types)+
		len(q.Relations) == 0 {
		return "", nil
	}
	var source, target, fn, kind string
	err := g.q.QueryRow(ctx, volatileHiddenCastSQL).Scan(&source, &target, &fn, &kind)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read implicit casts: %w", err)
	}
	return fmt.Sprintf("may run volatile function %s through the %s cast %s to %s, "+
		"which the query text does not show", fn, kind, source, target), nil
}

// argDefaultsSQL renders the default argument expressions of every
// function a called name can resolve to (same matching as
// functionResolutionSQL).
const argDefaultsSQL = `
SELECT f.schema, f.name, pg_catalog.pg_get_expr(pr.proargdefaults, 0)
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS f(schema, name)
JOIN pg_catalog.pg_proc pr ON pr.proname = f.name AND pr.proargdefaults IS NOT NULL
JOIN pg_catalog.pg_namespace n ON n.oid = pr.pronamespace
WHERE CASE WHEN f.schema = '' THEN n.nspname = ANY (pg_catalog.current_schemas(true))
	ELSE n.nspname = f.schema END`

type argDefault struct {
	fn   sqlast.QualifiedName
	expr string
}

// checkArgDefaults checks every default expression of the called
// functions (and of attribute notation, x.f being f(x)) as a query of its
// own; an omitted argument evaluates its default in the caller.
func (g *analyzeGuard) checkArgDefaults(
	ctx context.Context, q sqlast.ReadQuery, depth int,
) (string, error) {
	called := append(append([]sqlast.QualifiedName{}, q.Functions...), q.AttributeCalls...)
	if len(called) == 0 {
		return "", nil
	}
	defaults, err := g.argDefaults(ctx, called)
	if err != nil || len(defaults) == 0 {
		return "", err
	}
	if depth >= maxViewDepth {
		return "calls " + defaults[0].fn.String() +
			", whose default arguments are nested deeper than the guard follows", nil
	}
	for _, d := range defaults {
		reason, err := g.check(ctx, "SELECT "+d.expr, depth+1)
		if err != nil {
			return "", err
		}
		if reason != "" {
			return "calls " + d.fn.String() + ", whose default argument " + reason, nil
		}
	}
	return "", nil
}

func (g *analyzeGuard) argDefaults(
	ctx context.Context, called []sqlast.QualifiedName,
) ([]argDefault, error) {
	schemas, objects := splitNames(called)
	rows, err := g.q.Query(ctx, argDefaultsSQL, schemas, objects)
	if err != nil {
		return nil, fmt.Errorf("read default arguments of %s: %w", joinNames(called), err)
	}
	defer rows.Close()
	var out []argDefault
	for rows.Next() {
		var d argDefault
		if err := rows.Scan(&d.fn.Schema, &d.fn.Name, &d.expr); err != nil {
			return nil, fmt.Errorf("scan default arguments: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read default arguments of %s: %w", joinNames(called), err)
	}
	return out, nil
}
