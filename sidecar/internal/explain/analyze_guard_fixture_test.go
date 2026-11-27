package explain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fixture and assertions shared by the analyze guard's live tests: the
// cgo tests that use the real parser and the catalog tests that run on
// every build with a recorded parse shape.

// guardFixtureSQL starts with the cleanup, so a run that was stopped
// before its cleanup does not fail the next run's setup. The cleanup drops
// eg_dom with CASCADE: a stopped domain check leaves eg_safe_domarg on it.
const guardFixtureSQL = guardCleanupSQL + `
CREATE SCHEMA eg_shadow;
CREATE FUNCTION eg_shadow.lower(text) RETURNS text LANGUAGE sql VOLATILE AS 'SELECT $1';
CREATE TABLE public.eg_probe (v int);
INSERT INTO public.eg_probe VALUES (42);
CREATE SEQUENCE public.eg_seq;
CREATE OR REPLACE FUNCTION public.eg_volatile() RETURNS int LANGUAGE sql VOLATILE AS 'SELECT 1';
CREATE OR REPLACE FUNCTION public.eg_stable() RETURNS int LANGUAGE sql STABLE AS 'SELECT 2';
CREATE OR REPLACE FUNCTION public.upper(text) RETURNS text LANGUAGE sql VOLATILE AS 'SELECT $1';
CREATE VIEW public.eg_kill_view AS SELECT pg_terminate_backend(pid) AS k
	FROM pg_stat_activity WHERE application_name = 'eg_victim';
CREATE VIEW public.eg_nested_view AS SELECT * FROM public.eg_kill_view;
CREATE VIEW public.eg_safe_view AS SELECT v FROM public.eg_probe;
CREATE TABLE public.eg_rls (v int);
ALTER TABLE public.eg_rls ENABLE ROW LEVEL SECURITY;
CREATE OR REPLACE FUNCTION public.eg_volop(int, int) RETURNS bool LANGUAGE sql VOLATILE
	AS 'SELECT $1 = $2';
CREATE OPERATOR public.=== (LEFTARG = int, RIGHTARG = int, FUNCTION = public.eg_volop);
CREATE DOMAIN public.eg_dom AS int CHECK (VALUE > 0);
CREATE OR REPLACE FUNCTION public.eg_attr(public.eg_probe) RETURNS int LANGUAGE sql VOLATILE
	AS 'SELECT 1';
CREATE OR REPLACE FUNCTION public.eg_sfunc(int, int) RETURNS int LANGUAGE sql VOLATILE
	AS 'SELECT $1 + $2';
CREATE AGGREGATE public.eg_agg(int) (SFUNC = public.eg_sfunc, STYPE = int, INITCOND = '0');
`

const guardCleanupSQL = `
DROP SCHEMA IF EXISTS eg_shadow CASCADE;
DROP VIEW IF EXISTS public.eg_nested_view, public.eg_kill_view, public.eg_safe_view;
DROP AGGREGATE IF EXISTS public.eg_agg(int);
DROP OPERATOR IF EXISTS public.=== (int, int);
DROP TABLE IF EXISTS public.eg_probe, public.eg_rls CASCADE;
DROP SEQUENCE IF EXISTS public.eg_seq;
DROP DOMAIN IF EXISTS public.eg_dom CASCADE;
DROP FUNCTION IF EXISTS public.eg_volatile(), public.eg_stable(), public.upper(text),
	public.eg_volop(int, int), public.eg_sfunc(int, int);
`

// victim is a separate session that pg_terminate_backend would kill.
func openVictim(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	cfg := pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "eg_victim"
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect victim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func assertVictimAlive(t *testing.T, victim *pgx.Conn) {
	t.Helper()
	if err := victim.Ping(context.Background()); err != nil {
		t.Fatalf("victim session was terminated: %v", err)
	}
}

func explainFresh(t *testing.T, ex *Explainer, query string) *ExplainResult {
	t.Helper()
	_, _ = ex.pool.Exec(context.Background(), "DELETE FROM sage.explain_results")
	start := time.Now()
	result, err := ex.Explain(context.Background(), ExplainRequest{Query: query})
	if err != nil {
		t.Fatalf("Explain(%q): %v", query, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Explain(%q) took %v", query, time.Since(start))
	}
	return result
}

func assertRefused(t *testing.T, ex *Explainer, query, reason string) {
	t.Helper()
	result := explainFresh(t, ex, query)
	if result.ActualTimeMs != nil {
		t.Errorf("ANALYZE executed %q", query)
	}
	if !strings.Contains(result.AnalyzeRefused, reason) {
		t.Errorf("AnalyzeRefused = %q, want it to mention %q", result.AnalyzeRefused, reason)
	}
	if !strings.Contains(result.Note, "without ANALYZE") || len(result.PlanJSON) == 0 {
		t.Errorf("plan-only fallback missing: note=%q plan=%s", result.Note, result.PlanJSON)
	}
}

func assertAnalyzed(t *testing.T, ex *Explainer, query string) {
	t.Helper()
	result := explainFresh(t, ex, query)
	if result.ActualTimeMs == nil || result.AnalyzeRefused != "" {
		t.Errorf("%q not analyzed: refused=%q", query, result.AnalyzeRefused)
	}
}

func assertSideEffectsAbsent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var pwned, rows int
	var seqCalled bool
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM pg_stat_activity WHERE application_name = 'eg_pwned'),
		(SELECT count(*) FROM public.eg_probe),
		(SELECT is_called FROM public.eg_seq)`).Scan(&pwned, &rows, &seqCalled)
	if err != nil {
		t.Fatalf("read side effects: %v", err)
	}
	if pwned != 0 || rows != 1 || seqCalled {
		t.Errorf("side effects: pwned sessions=%d probe rows=%d seq called=%v",
			pwned, rows, seqCalled)
	}
}

// hiddenFixtureSQL builds calls a query does not show: a stable function
// whose default argument takes an advisory lock, a constant default, a
// function whose volatility a test flips, and a chain of five views.
const hiddenFixtureSQL = `
DROP VIEW IF EXISTS public.eg_chain1, public.eg_chain2, public.eg_chain3,
	public.eg_chain4, public.eg_chain5;
CREATE OR REPLACE FUNCTION public.eg_defaulted(a bool DEFAULT pg_try_advisory_lock(4243))
	RETURNS bool LANGUAGE plpgsql STABLE AS 'BEGIN RETURN a; END';
CREATE OR REPLACE FUNCTION public.eg_const_default(a int DEFAULT 7)
	RETURNS int LANGUAGE sql STABLE AS 'SELECT $1';
CREATE OR REPLACE FUNCTION public.eg_flip(int) RETURNS int LANGUAGE sql IMMUTABLE
	AS 'SELECT $1';
CREATE VIEW public.eg_chain5 AS SELECT v FROM public.eg_probe;
CREATE VIEW public.eg_chain4 AS SELECT v FROM public.eg_chain5;
CREATE VIEW public.eg_chain3 AS SELECT v FROM public.eg_chain4;
CREATE VIEW public.eg_chain2 AS SELECT v FROM public.eg_chain3;
CREATE VIEW public.eg_chain1 AS SELECT v FROM public.eg_chain2;
`

const hiddenCleanupSQL = `
DROP VIEW IF EXISTS public.eg_chain1, public.eg_chain2, public.eg_chain3,
	public.eg_chain4, public.eg_chain5;
DROP FUNCTION IF EXISTS public.eg_defaulted(bool), public.eg_const_default(int),
	public.eg_flip(int);
`

// castFixtureSQL adds an implicit cast through a volatile function. It
// applies to the whole database, so a test creates it and drops it.
const castFixtureSQL = castCleanupSQL + `;
CREATE TYPE public.eg_enum AS ENUM ('a');
CREATE TABLE public.eg_cast_t (x public.eg_enum);
INSERT INTO public.eg_cast_t VALUES ('a');
CREATE FUNCTION public.eg_enum_text(public.eg_enum) RETURNS text LANGUAGE sql VOLATILE
	AS 'SELECT pg_advisory_lock(4244)::text';
CREATE CAST (public.eg_enum AS text) WITH FUNCTION public.eg_enum_text(public.eg_enum)
	AS IMPLICIT;
`

// castCleanupSQL drops the table first: DROP TYPE ... CASCADE only drops
// the table's column, and the empty table would fail the next setup.
const castCleanupSQL = `DROP TABLE IF EXISTS public.eg_cast_t;
DROP TYPE IF EXISTS public.eg_enum CASCADE`

// assertNoAdvisoryLocks fails when any session holds one of the advisory
// locks the hidden-call fixtures take (4242 to 4244): a lock survives the
// ROLLBACK of the EXPLAIN transaction, so it proves the call ran.
func assertNoAdvisoryLocks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var held int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_catalog.pg_locks
		WHERE locktype = 'advisory' AND objid IN (4242, 4243, 4244)`).Scan(&held)
	if err != nil {
		t.Fatalf("read advisory locks: %v", err)
	}
	if held != 0 {
		t.Errorf("%d fixture advisory lock(s) held: a refused call ran", held)
	}
}

func execFixture(t *testing.T, pool *pgxpool.Pool, setup, cleanup string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), setup); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), cleanup) })
}
