package analyzer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
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

// partmanIDChildren creates an id-partitioned parent with n real children
// of 100 ids each (p1 starts at 0) and lists them in the stub, as
// pg_partman lists only children that exist.
func partmanIDChildren(t *testing.T, pool *pgxpool.Pool, q, parent string, n int) {
	t.Helper()
	partmanParentTable(t, pool, parent, "id", "bigint")
	for g := 1; g <= n; g++ {
		extExec(t, pool, fmt.Sprintf(`CREATE TABLE public.%s_p%d PARTITION OF public.%s
			FOR VALUES FROM (%d) TO (%d)`, parent, g, parent, (g-1)*100, g*100))
	}
	extExec(t, pool, `INSERT INTO `+q+`.stub_children
		SELECT 'public.' || $1, 'public', $1 || '_p' || g, g, NULL, (g - 1) * 100
		FROM generate_series(1, $2::int) g`, parent, n)
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
	partmanIDChildren(t, pool, q, "ext_pm_ids", 5)
	extExec(t, pool, `INSERT INTO public.ext_pm_ids SELECT generate_series(1, 150)`)
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_ids', 'id', 4)`)
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

// sqlRecorder records the statements a reading runs.
type sqlRecorder struct {
	mu  sync.Mutex
	sql []string
}

func (r *sqlRecorder) reader(pool *pgxpool.Pool) catalogread.Reader {
	rd := catalogread.New(pool, catalogread.Default())
	rd.Done = func(_ context.Context, s catalogread.ReadStat) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sql = append(r.sql, s.SQL)
	}
	return rd
}

// reads reports whether a statement read FROM table, exactly that table.
func (r *sqlRecorder) reads(table string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sql {
		if strings.HasSuffix(strings.TrimSpace(s), "FROM "+table) {
			return true
		}
	}
	return false
}

func TestExtPartman_IDMaxReadsOnlyListedChildren(t *testing.T) {
	a, pool, _ := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: "partman"}})
	q := partmanStub(t, pool, "partman")
	partmanIDChildren(t, pool, q, "ext_pm_scan", 8)
	extExec(t, pool, `INSERT INTO public.ext_pm_scan SELECT generate_series(1, 50)`)
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_scan', 'id', 2)`)
	read := func() (*sqlRecorder, []Finding) {
		rec := &sqlRecorder{}
		a.eval = newCycleEval()
		a.readExtensions(context.Background(), rec.reader(pool))
		_ = a.extras.takeUndecided()
		return rec, rulePartmanPremakeGap(nil, nil, nil, a.extras)
	}
	// Rows only in p1, older than the newest premake+1 (p8, p7, p6): all
	// three listed children are future, and neither p1 nor the parent is read.
	rec, got := read()
	if len(got) != 0 {
		t.Fatalf("rows below the listed children = %+v, want none", got)
	}
	for _, tbl := range []string{"public.ext_pm_scan", "public.ext_pm_scan_p1",
		"public.ext_pm_scan_p5"} {
		if rec.reads(tbl) {
			t.Fatalf("read %s; want only the listed children: %v", tbl, rec.sql)
		}
	}
	if !rec.reads("public.ext_pm_scan_p6") {
		t.Fatalf("p6 (the oldest listed child) not read: %v", rec.sql)
	}
	extExec(t, pool, `INSERT INTO public.ext_pm_scan VALUES (650)`)
	rec, got = read()
	if len(got) != 1 || got[0].Detail["future_partitions"] != 1 {
		t.Fatalf("max id 650 in p7 with premake 2 = %+v, want one finding, 1 future", got)
	}
	if rec.reads("public.ext_pm_scan_p6") {
		t.Fatalf("read p6 after p7 held the max: %v", rec.sql)
	}
}

func TestExtPartman_UnreadableChildKeepsFindingOpen(t *testing.T) {
	a, pool, logs := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: "partman"}})
	q := partmanStub(t, pool, "partman")
	partmanIDChildren(t, pool, q, "ext_pm_gone", 5)
	extExec(t, pool, `INSERT INTO public.ext_pm_gone SELECT generate_series(1, 150)`)
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_gone', 'id', 4)`)
	if got := byCategory(runExtCycle(t, a), categoryPartmanPremake); len(got) != 1 {
		t.Fatalf("premake gap = %+v, want one", got)
	}
	// The newest listed child cannot be read: no verdict for this parent.
	extExec(t, pool, `DROP TABLE public.ext_pm_gone_p5`)
	if got := byCategory(runExtCycle(t, a), categoryPartmanPremake); len(got) != 0 {
		t.Fatalf("an unreadable child gave %+v", got)
	}
	if n := countFindingRows(t, pool, categoryPartmanPremake, "public.ext_pm_gone",
		"open"); n != 1 {
		t.Fatalf("open rows after an unreadable child = %d, want 1 (kept open)", n)
	}
	if n := logs.warns("ext_pm_gone_p5"); n != 1 {
		t.Fatalf("WARN lines naming the unreadable child = %d, want 1", n)
	}
}

func TestExtPartman_UnreadableDefaultKeepsFindingOpen(t *testing.T) {
	a, pool, logs := extAnalyzer(t, map[string]extInstall{extPartman: {Schema: "partman"}})
	q := partmanStub(t, pool, "partman")
	partmanParentTable(t, pool, "ext_pm_hid", "id", "bigint")
	extExec(t, pool, `CREATE TABLE public.ext_pm_hid_default PARTITION OF public.ext_pm_hid
		DEFAULT`)
	extExec(t, pool, `INSERT INTO public.ext_pm_hid VALUES (7)`)
	extExec(t, pool, `INSERT INTO `+q+`.part_config (parent_table, control, premake)
		VALUES ('public.ext_pm_hid', 'id', 0)`)
	if got := byCategory(runExtCycle(t, a), categoryPartmanDefault); len(got) != 1 {
		t.Fatalf("default partition with a row = %+v, want one", got)
	}
	// A role that reads part_config but not the default partition.
	role := partmanConfigReader(t, pool, q)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, "RESET ROLE")
		conn.Release()
	}()
	if _, err := conn.Exec(ctx, "SET ROLE "+role); err != nil {
		t.Fatalf("set role: %v", err)
	}
	got := runExtCycleWith(t, a, func(ctx context.Context) {
		a.readExtensions(ctx, catalogread.New(conn, catalogread.Default()))
	})
	if f := byCategory(got, categoryPartmanDefault); len(f) != 0 {
		t.Fatalf("an unreadable default partition gave %+v", f)
	}
	if !a.eval.ok[categoryPartmanDefault] || a.eval.hasFailed(categoryPartmanDefault) {
		t.Fatal("partman_default_rows not evaluated: the hold must be per partition")
	}
	n := countFindingRows(t, pool, categoryPartmanDefault, "public.ext_pm_hid_default", "open")
	if n != 1 {
		t.Fatalf("open rows after a denied read = %d, want 1 (kept open)", n)
	}
	if n := logs.warns("public.ext_pm_hid_default"); n != 1 {
		t.Fatalf("WARN lines naming the default partition = %d, want 1", n)
	}
}

// partmanConfigReader creates a role that reads part_config and nothing
// else in pg_partman's schema.
func partmanConfigReader(t *testing.T, pool *pgxpool.Pool, q string) string {
	t.Helper()
	role := "sage_ext_reader"
	_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+role)
	_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	extExec(t, pool, "CREATE ROLE "+role+" NOLOGIN")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	extExec(t, pool, "GRANT USAGE ON SCHEMA "+q+" TO "+role)
	extExec(t, pool, "GRANT SELECT ON "+q+".part_config TO "+role)
	return role
}
