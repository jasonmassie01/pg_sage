package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// domainFixtureSQL builds domain coercions a query does not show: a domain
// whose CHECK takes advisory lock 4243, reached as a function argument, an
// array element argument, an operator operand and a composite field, plus
// a function over the safe fixture domain eg_dom (CHECK (VALUE > 0)).
const domainFixtureSQL = `
DROP DOMAIN IF EXISTS public.eg_lockdom CASCADE;
CREATE DOMAIN public.eg_lockdom AS int CHECK (pg_try_advisory_lock(4243) IS NOT NULL);
CREATE FUNCTION public.eg_domarg(x public.eg_lockdom) RETURNS int LANGUAGE plpgsql STABLE
	AS 'BEGIN RETURN 1; END';
CREATE FUNCTION public.eg_arrdom(x public.eg_lockdom[]) RETURNS int LANGUAGE plpgsql STABLE
	AS 'BEGIN RETURN 1; END';
CREATE FUNCTION public.eg_domop_fn(public.eg_lockdom, int) RETURNS bool LANGUAGE plpgsql
	STABLE AS 'BEGIN RETURN true; END';
CREATE OPERATOR public.=#= (LEFTARG = public.eg_lockdom, RIGHTARG = int,
	FUNCTION = public.eg_domop_fn);
CREATE TYPE public.eg_comp AS (a public.eg_lockdom);
CREATE FUNCTION public.eg_safe_domarg(x public.eg_dom) RETURNS int LANGUAGE plpgsql STABLE
	AS 'BEGIN RETURN 1; END';
`

const domainCleanupSQL = `
DROP DOMAIN IF EXISTS public.eg_lockdom CASCADE;
DROP FUNCTION IF EXISTS public.eg_safe_domarg(public.eg_dom);
`

// domainShapes are the parse shapes of the domain tests' queries and of
// the CHECK expressions the guard renders.
var domainShapes = map[string]sqlast.ReadQuery{
	"SELECT eg_domarg(1)":           {Functions: names(qn("", "eg_domarg"))},
	"SELECT eg_arrdom(ARRAY[1])":    {Functions: names(qn("", "eg_arrdom"))},
	"SELECT 1 =#= 2":                {Operators: names(qn("", "=#="))},
	"SELECT ROW(1)::public.eg_comp": {Types: names(qn("public", "eg_comp"))},
	"SELECT eg_safe_domarg(1)":      {Functions: names(qn("", "eg_safe_domarg"))},
	"SELECT (VALUE > 0)":            {Operators: names(qn("", ">"))},
	"SELECT (pg_try_advisory_lock((4243)::bigint) IS NOT NULL)": {
		Functions: names(qn("", "pg_try_advisory_lock")),
		Types:     names(qn("pg_catalog", "int8"))},
}

// A coercion to a domain runs its CHECK in the caller: each query below
// would take advisory lock 4243 under ANALYZE (shown on PostgreSQL 17).
func checkGuardRefusesDomainChecks(t *testing.T, inspect inspectFn) {
	pool := liveExplainPool(t)
	execFixture(t, pool, guardFixtureSQL, guardCleanupSQL)
	execFixture(t, pool, domainFixtureSQL, domainCleanupSQL)
	ex := guardExplainer(pool, 5000, inspect)
	for query, reason := range map[string]string{
		"SELECT eg_domarg(1)":           "calls eg_domarg, which coerces to domain eg_lockdom",
		"SELECT eg_arrdom(ARRAY[1])":    "calls eg_arrdom, which coerces to domain eg_lockdom",
		"SELECT 1 =#= 2":                "uses operator =#=, which coerces to domain eg_lockdom",
		"SELECT ROW(1)::public.eg_comp": "casts to public.eg_comp, which coerces to domain",
	} {
		t.Run(query, func(t *testing.T) {
			assertRefused(t, ex, query, reason)
			assertRefused(t, ex, query, "CHECK constraint calls volatile function "+
				"pg_try_advisory_lock")
			assertNoAdvisoryLocks(t, pool)
		})
	}
	assertAnalyzed(t, ex, "SELECT eg_safe_domarg(1)")
	assertSideEffectsAbsent(t, pool)
}

func TestGuardCatalogRefusesDomainChecks(t *testing.T) {
	checkGuardRefusesDomainChecks(t, recordedInspector())
}

// A failed CHECK lookup refuses ANALYZE and names what failed.
func TestAnalyzeRefusalDomainLookupFailsClosed(t *testing.T) {
	q := &scriptedQuerier{failOn: "contypid", err: errors.New("permission denied")}
	ex := New(nil, liveConfig(5000), noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{Functions: names(qn("", "f"))}, nil
	}
	got := ex.analyzeRefusal(context.Background(), q, "SELECT f()")
	for _, want := range []string{"could not verify the query against the catalog",
		"domain CHECK constraints", "permission denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal = %q, want it to mention %q", got, want)
		}
	}
}

// domainRowsQuerier answers the CHECK lookup with one row and counts it.
type domainRowsQuerier struct{ reads int }

func (d *domainRowsQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	d.reads++
	return &oneDomainRow{}, nil
}

func (d *domainRowsQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{errors.New("unexpected QueryRow")}
}

type oneDomainRow struct {
	pgx.Rows
	done bool
}

func (r *oneDomainRow) Next() bool { ok := !r.done; r.done = true; return ok }
func (r *oneDomainRow) Close()     {}
func (r *oneDomainRow) Err() error { return nil }
func (r *oneDomainRow) Scan(dest ...any) error {
	vals := []string{"operator", "public", "=#=", "eg_lockdom", "VALUE > 0"}
	for i, v := range vals {
		*dest[i].(*string) = v
	}
	return nil
}

// At the depth cap the CHECK is refused unread; a literal-only shape makes
// no lookup; every named kind is a seed.
func TestDomainChecksBoundaries(t *testing.T) {
	d := &domainRowsQuerier{}
	g := &analyzeGuard{q: d, inspect: func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{}, errors.New("must not parse past the cap")
	}}
	shape := sqlast.ReadQuery{Operators: names(qn("public", "=#="))}
	got, err := g.checkDomainChecks(context.Background(), shape, maxViewDepth)
	want := "uses operator public.=#=, which coerces to domain eg_lockdom, whose CHECK " +
		"constraints are nested deeper than the guard follows"
	if err != nil || got != want {
		t.Errorf("at the cap: %q, %v; want %q", got, err, want)
	}
	got, err = g.checkDomainChecks(context.Background(), shape, maxViewDepth-1)
	if err != nil || !strings.Contains(got, "could not inspect the query: must not parse") {
		t.Errorf("below the cap the CHECK must be inspected: %q, %v", got, err)
	}
	before := d.reads
	if got, err := g.checkDomainChecks(context.Background(), sqlast.ReadQuery{
		Relations: names(qn("", "r"))}, 0); got != "" || err != nil || d.reads != before {
		t.Errorf("relation-only shape: %q, %v, %d reads", got, err, d.reads-before)
	}
	kinds, schemas, objects := domainSeeds(sqlast.ReadQuery{
		Functions: names(qn("s", "f")), AttributeCalls: names(qn("", "a")),
		Operators: names(qn("", "=")), Types: names(qn("", "t"))})
	if strings.Join(kinds, ",") != "function,function,operator,type" ||
		strings.Join(schemas, ",") != "s,,," || strings.Join(objects, ",") != "f,a,=,t" {
		t.Errorf("seeds = %v %v %v", kinds, schemas, objects)
	}
}
