package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// market-16 pg_partman checks on real PostgreSQL, against a stub schema of
// pg_partman's shapes (see rules_ext_db_test.go).
// No concurrent access tests: the extension reading belongs to one cycle on
// the analyzer's cycle goroutine.

// partmanStub creates a pg_partman-shaped schema and returns its quoted name.
func partmanStub(t *testing.T, pool *pgxpool.Pool, schema string) string {
	t.Helper()
	q := sqlident.Ident(schema)
	extExec(t, pool, "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	extExec(t, pool, "CREATE SCHEMA "+q)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	extExec(t, pool, `CREATE TABLE `+q+`.part_config (parent_table text PRIMARY KEY,
		control text NOT NULL, partition_interval text NOT NULL DEFAULT '1 day',
		partition_type text NOT NULL DEFAULT 'range', premake int NOT NULL DEFAULT 4)`)
	extExec(t, pool, `CREATE TABLE `+q+`.stub_children (parent text, schemaname text,
		tablename text, ord int, start_time timestamptz, start_id bigint)`)
	extExec(t, pool, `CREATE FUNCTION `+q+`.show_partitions(p_parent_table text,
		p_order text DEFAULT 'ASC', p_include_default boolean DEFAULT false)
		RETURNS TABLE (partition_schemaname text, partition_tablename text)
		LANGUAGE sql STABLE AS $$
		SELECT schemaname, tablename FROM `+q+`.stub_children
		WHERE parent = p_parent_table
		ORDER BY CASE WHEN p_order = 'DESC' THEN -ord ELSE ord END $$`)
	extExec(t, pool, `CREATE FUNCTION `+q+`.show_partition_info(p_child_table text,
		p_partition_interval text DEFAULT NULL, p_parent_table text DEFAULT NULL,
		OUT child_start_time timestamptz, OUT child_end_time timestamptz,
		OUT child_start_id bigint, OUT child_end_id bigint, OUT suffix text)
		LANGUAGE sql STABLE AS $$
		SELECT start_time, NULL::timestamptz, start_id, NULL::bigint, NULL::text
		FROM `+q+`.stub_children WHERE schemaname || '.' || tablename = p_child_table $$`)
	return q
}

func partmanParentTable(t *testing.T, pool *pgxpool.Pool, name, col, typ string) {
	t.Helper()
	extExec(t, pool, "DROP TABLE IF EXISTS public."+name+" CASCADE")
	extExec(t, pool, fmt.Sprintf(`CREATE TABLE public.%s (%s %s NOT NULL)
		PARTITION BY RANGE (%s)`, name, col, typ, col))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+name+" CASCADE")
	})
}

func TestExtPartman_PremakeBoundary(t *testing.T) {
	a, pool, _ := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: "partman"}})
	q := partmanStub(t, pool, "partman")
	partmanParentTable(t, pool, "ext_pm_events", "at", "timestamptz")
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_events', 'at', 4)`)
	extExec(t, pool, `INSERT INTO `+q+`.stub_children
		SELECT 'public.ext_pm_events', 'public', 'ext_pm_events_p' || g, g,
		       $1::timestamptz + (g - 2) * interval '1 day', NULL
		FROM generate_series(1, 6) g`, extTestNow.Add(-time.Hour))
	// Children start at now-1h-1d, now-1h, then +23h..+95h: four future.
	if got := byCategory(runExtCycle(t, a), categoryPartmanPremake); len(got) != 0 {
		t.Fatalf("premake 4 with 4 future children = %+v", got)
	}
	extExec(t, pool, `DELETE FROM `+q+`.stub_children WHERE ord = 6`)
	got := byCategory(runExtCycle(t, a), categoryPartmanPremake)
	if len(got) != 1 || got[0].Detail["future_partitions"] != 3 ||
		got[0].ObjectIdentifier != "public.ext_pm_events" ||
		got[0].Detail["control_kind"] != "time" {
		t.Fatalf("premake 4 with 3 future children = %+v", got)
	}
}

func TestExtPartman_IDControlInQuotedSchema(t *testing.T) {
	schema := `My "pm"`
	a, pool, _ := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: schema}})
	q := partmanStub(t, pool, schema)
	partmanParentTable(t, pool, "ext_pm_ids", "id", "bigint")
	extExec(t, pool, `CREATE TABLE public.ext_pm_ids_p0 PARTITION OF public.ext_pm_ids
		FOR VALUES FROM (0) TO (1000)`)
	extExec(t, pool, `INSERT INTO public.ext_pm_ids SELECT generate_series(1, 150)`)
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_ids', 'id', 4)`)
	extExec(t, pool, `INSERT INTO `+q+`.stub_children
		SELECT 'public.ext_pm_ids', 'public', 'ext_pm_ids_p' || g, g, NULL, (g - 1) * 100
		FROM generate_series(1, 5) g`)
	// Starts 0..400 with ids up to 150: 200, 300 and 400 are future.
	got := byCategory(runExtCycle(t, a), categoryPartmanPremake)
	if len(got) != 1 || got[0].Detail["future_partitions"] != 3 ||
		got[0].Detail["control_kind"] != "id" {
		t.Fatalf("id-controlled premake gap = %+v", got)
	}
}

func TestExtPartman_DefaultPartitionRows(t *testing.T) {
	a, pool, _ := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: "partman"}})
	q := partmanStub(t, pool, "partman")
	partmanParentTable(t, pool, "ext_pm_def", "id", "bigint")
	extExec(t, pool, `CREATE TABLE public.ext_pm_def_default PARTITION OF public.ext_pm_def
		DEFAULT`)
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_def', 'id', 0)`)
	if got := runExtCycle(t, a); len(got) != 0 {
		t.Fatalf("an empty default partition gave %+v", got)
	}
	if !a.eval.ok[categoryPartmanDefault] || a.eval.hasFailed(categoryPartmanDefault) {
		t.Fatal("partman_default_rows not evaluated")
	}
	extExec(t, pool, `INSERT INTO public.ext_pm_def VALUES (42)`)
	got := byCategory(runExtCycle(t, a), categoryPartmanDefault)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.ext_pm_def_default" ||
		got[0].Detail["parent_table"] != "public.ext_pm_def" {
		t.Fatalf("default partition with a row = %+v", got)
	}
	extExec(t, pool, `DELETE FROM public.ext_pm_def`)
	runExtCycle(t, a)
	n := countFindingRows(t, pool, categoryPartmanDefault, "public.ext_pm_def_default",
		"resolved")
	if n != 1 {
		t.Fatalf("resolved rows after the rows moved out = %d, want 1", n)
	}
}

func TestExtPartman_MissingPartConfigIsNotEvaluated(t *testing.T) {
	a, pool, logs := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: "no_such_pm"}})
	_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS no_such_pm CASCADE")
	if got := runExtCycle(t, a); len(got) != 0 {
		t.Fatalf("a missing part_config gave %+v", got)
	}
	for _, c := range []string{categoryPartmanPremake, categoryPartmanDefault} {
		if !a.eval.hasFailed(c) {
			t.Fatalf("%s evaluated without part_config", c)
		}
	}
	if n := logs.warns("part_config"); n != 1 {
		t.Fatalf("WARN lines naming part_config = %d, want 1", n)
	}
}
