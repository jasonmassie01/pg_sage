package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// CatalogQuerier is the catalog access a read proof needs: the connection
// that will run the statement, so names resolve on its search_path.
type CatalogQuerier = catalogQuerier

// ReadProofOptions adapts the analyze guard for the brokered read path
// (spec §6.8 S3). The zero value is the analyze guard itself.
type ReadProofOptions struct {
	// AllowRLS admits relations with row-level security: the reading role
	// is NOBYPASSRLS, so the policies apply. A relation is still refused
	// when one of its policy expressions is not provably side-effect free.
	AllowRLS bool
	// DenySecurityDefiner refuses SECURITY DEFINER functions: they run
	// with their owner's privileges, not the reader's.
	DenySecurityDefiner bool
	// DenyFunction is an extra name deny-list, checked before the catalog.
	DenyFunction func(sqlast.QualifiedName) bool
	// AllowCatalogRelation, when set, is the readable-catalog allowlist
	// for relations in pg_catalog and information_schema; an allowed
	// system view is trusted as defined.
	AllowCatalogRelation func(schema, name string) bool
}

// ProveRead checks that executing sql can invoke nothing but provably
// side-effect-free functions, operators, casts and relations, under opts.
// It returns why the statement is refused, or "" when it is proven. An
// error is a failed catalog read: the caller must refuse (fail closed).
// inspect is nil for the libpg_query parser.
func ProveRead(ctx context.Context, q CatalogQuerier, sql string,
	inspect func(string) (sqlast.ReadQuery, error), opts ReadProofOptions) (string, error) {
	if inspect == nil {
		inspect = sqlast.InspectReadQuery
	}
	g := &analyzeGuard{inspect: inspect, q: q, opts: opts}
	return g.check(ctx, sql, 0)
}

// deniedByName applies the extra deny-list to called functions and to
// attribute notation (x.f is f(x) when x has no column f).
func (g *analyzeGuard) deniedByName(shape sqlast.ReadQuery) string {
	if g.opts.DenyFunction == nil {
		return ""
	}
	for _, list := range [][]sqlast.QualifiedName{shape.Functions, shape.AttributeCalls} {
		for _, fn := range list {
			if g.opts.DenyFunction(fn) {
				return "calls " + fn.String() + ", which agent reads may not call"
			}
		}
	}
	return ""
}

// functionDefinerSQL counts, per name, the functions it can resolve to
// and how many of them are SECURITY DEFINER (in the unsafe column).
const functionDefinerSQL = `
SELECT f.schema, f.name, pg_catalog.count(p.oid)::int,
	pg_catalog.count(*) FILTER (WHERE p.prosecdef)::int
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[])) AS f(schema, name)
LEFT JOIN LATERAL (
	SELECT pr.oid, pr.prosecdef FROM pg_catalog.pg_proc pr
	JOIN pg_catalog.pg_namespace n ON n.oid = pr.pronamespace
	WHERE pr.proname = f.name AND CASE WHEN f.schema = ''
		THEN n.nspname = ANY (pg_catalog.current_schemas(true))
		ELSE n.nspname = f.schema END
) p ON true
GROUP BY f.schema, f.name`

func (g *analyzeGuard) checkDefiners(ctx context.Context, q sqlast.ReadQuery) (string, error) {
	if !g.opts.DenySecurityDefiner || len(q.Functions) == 0 {
		return "", nil
	}
	found, err := resolveNames(ctx, g.q, "function owners", functionDefinerSQL, q.Functions)
	if err != nil {
		return "", err
	}
	for _, fn := range q.Functions {
		if found[fn].unsafe > 0 {
			return "calls " + fn.String() + ", a SECURITY DEFINER function", nil
		}
	}
	return "", nil
}

// catalogDecision applies the readable-catalog allowlist; decided is false
// for a relation outside the system schemas or with no allowlist set.
func (g *analyzeGuard) catalogDecision(rel sqlast.QualifiedName,
	info relationInfo) (string, bool) {
	if g.opts.AllowCatalogRelation == nil || info.nsp == nil {
		return "", false
	}
	nsp := *info.nsp
	if nsp != "pg_catalog" && nsp != "information_schema" {
		return "", false
	}
	name := rel.Name
	if g.opts.AllowCatalogRelation(nsp, name) {
		return "", true
	}
	return fmt.Sprintf("reads %s.%s, which is not in the readable catalog allowlist",
		nsp, name), true
}

// tableRefusal judges a resolved non-view relation; with AllowRLS a table
// with row-level security is judged by its policy expressions.
func (g *analyzeGuard) tableRefusal(ctx context.Context, rel sqlast.QualifiedName,
	info relationInfo, depth int) (string, error) {
	rls := info.rls != nil && *info.rls
	kind := *info.kind
	if !rls || !g.opts.AllowRLS || (kind != "r" && kind != "p") {
		return relationKindRefusal(rel, kind, rls), nil
	}
	return g.policyRefusal(ctx, rel, *info.oid, depth)
}

// policyRefusal checks every policy expression on a relation as if it
// were a query of its own.
func (g *analyzeGuard) policyRefusal(ctx context.Context, rel sqlast.QualifiedName,
	oid uint32, depth int) (string, error) {
	if depth >= maxViewDepth {
		return "reads " + rel.String() + ": policies nested deeper than the guard follows",
			nil
	}
	rows, err := g.q.Query(ctx, `SELECT pg_catalog.pg_get_expr(p.polqual, p.polrelid),
		pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid)
		FROM pg_catalog.pg_policy p WHERE p.polrelid = $1::oid`, oid)
	if err != nil {
		return "", fmt.Errorf("read policies of %s: %w", rel.String(), err)
	}
	var exprs []string
	for rows.Next() {
		var qual, check *string
		if err := rows.Scan(&qual, &check); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan policies of %s: %w", rel.String(), err)
		}
		for _, e := range []*string{qual, check} {
			if e != nil && strings.TrimSpace(*e) != "" {
				exprs = append(exprs, *e)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read policies of %s: %w", rel.String(), err)
	}
	for _, e := range exprs {
		reason, err := g.check(ctx, "SELECT "+e, depth+1)
		if err != nil {
			return "", err
		}
		if reason != "" {
			return "reads " + rel.String() + ", whose row-level security policy " + reason,
				nil
		}
	}
	return "", nil
}
