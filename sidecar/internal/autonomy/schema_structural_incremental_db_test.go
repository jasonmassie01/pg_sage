package autonomy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schemaguard"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The incremental structural scan against real PostgreSQL: after every
// kind of DDL the answer equals a full scan of the catalog, and only the
// tables the DDL touched have their columns aggregated again.

// extensionMemberFilter keeps out the tables an extension owns
// (hint_plan.hints): they are the extension's to define.
const extensionMemberFilter = `
      AND NOT EXISTS (SELECT 1 FROM pg_depend dep WHERE dep.classid='pg_class'::regclass
        AND dep.objid=tbl.oid AND dep.deptype='e')`

// structuralReferenceSQL is the full structural scan (v2.3.1's aggregate,
// temporary and extension tables excluded): every pass must answer
// exactly its rows.
const structuralReferenceSQL = `
WITH tables AS (
    SELECT ns.nspname AS schema_name, tbl.relname AS table_name,
           count(*) AS column_count,
           count(*) FILTER (WHERE typ.typname IN ('text','varchar')) AS text_count,
           array_agg(att.attname) FILTER (WHERE typ.typname IN ('text','varchar')
             AND (att.attname='count_text' OR att.attname ~ '(_id|_count|_number)$'))
             AS tightening
    FROM pg_class tbl
    JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
    JOIN pg_attribute att ON att.attrelid=tbl.oid
      AND att.attnum>0 AND NOT att.attisdropped
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE tbl.relkind IN ('r','p') AND tbl.relpersistence <> 't'
      AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')` +
	extensionMemberFilter + `
    GROUP BY tbl.oid, ns.nspname, tbl.relname
)
SELECT schema_name, table_name, ''::name AS column_name, 'everything_text' AS kind
FROM tables WHERE column_count>=3 AND text_count=column_count
UNION ALL
SELECT schema_name, table_name, unnest(tightening), 'type_tightening' AS kind
FROM tables WHERE tightening IS NOT NULL
ORDER BY 1,2,4,3`

// userTablesSQL counts the tables a full pass aggregates.
const userTablesSQL = `SELECT count(*) FROM pg_class tbl
JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
WHERE tbl.relkind IN ('r','p') AND tbl.relpersistence <> 't'
  AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')` +
	extensionMemberFilter

func rowsOf(t *testing.T, dsn, sql string) []string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("reference scan: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s, tb, c, k string
		if err := rows.Scan(&s, &tb, &c, &k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", s, tb, c, k))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

func userTables(t *testing.T, dsn string) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(), userTablesSQL).Scan(&n); err != nil {
		t.Fatalf("count user tables: %v", err)
	}
	return n
}

func structuralAnswer(t *testing.T, d postgresSchemaDetector) []string {
	t.Helper()
	items, err := d.detectStructuralPathologies(context.Background())
	if err != nil {
		t.Fatalf("structural pass: %v", err)
	}
	out := []string{}
	for _, item := range items {
		out = append(out, invariantRow(item))
		if item.Kind == schemaguard.InvariantTypeTightening && item.ProposedSQL !=
			typeTighteningProposal(item.Schema, item.Table, item.Subject) {
			t.Errorf("%s: proposal %q", invariantRow(item), item.ProposedSQL)
		}
	}
	return out
}

func assertReference(t *testing.T, dsn, when string, got []string) {
	t.Helper()
	want := rowsOf(t, dsn, structuralReferenceSQL)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s: the pass differs from a full scan:\n got %q\nwant %q", when, got, want)
	}
}

// passTrace is what the recorded statements show of the passes since the
// last trace: passes run, column-version checks, and the tables each
// column aggregation read.
type passTrace struct {
	passes, versions int
	aggregated       []int
}

func (p passTrace) reaggregated() int {
	n := 0
	for _, a := range p.aggregated {
		n += a
	}
	return n
}

func tracePasses(t *testing.T, rec *testdb.QueryRecorder) passTrace {
	t.Helper()
	tr := passTrace{passes: len(rec.Matching("structural:tables")),
		versions: len(rec.Matching("structural:versions"))}
	for _, q := range rec.Matching("structural:columns") {
		oids, ok := q.Args[0].([]uint32)
		if !ok {
			t.Fatalf("column aggregation argument is %T, want []uint32", q.Args[0])
		}
		tr.aggregated = append(tr.aggregated, len(oids))
	}
	rec.Reset()
	return tr
}

const incrementalFixture = `CREATE SCHEMA inc_a;
CREATE TABLE inc_a.orders (account_id text, n int);
CREATE TABLE inc_a.notes (a text, b text, c text);
CREATE TABLE inc_a.typed (id int, label text)`

// allTables marks a step whose pass aggregates every user table.
const allTables = -1

type ddlStep struct {
	name         string
	sql          string
	reaggregated int  // tables whose columns the pass aggregates again
	verified     bool // the pass checks every table's column versions
}

var incrementalSteps = []ddlStep{
	{"create table", `CREATE TABLE inc_a.refunds (order_id text, amount int)`, 1, false},
	{"add column", `ALTER TABLE inc_a.typed ADD COLUMN user_id text`, 1, false},
	{"rename column", `ALTER TABLE inc_a.orders RENAME COLUMN account_id TO account`,
		1, true},
	{"drop column", `ALTER TABLE inc_a.notes DROP COLUMN c`, 1, true},
	{"alter column type with a rewrite", `ALTER TABLE inc_a.typed ALTER COLUMN id TYPE text`,
		1, true},
	{"alter column type without a rewrite", `CREATE DOMAIN inc_a.label_t AS text;
		ALTER TABLE inc_a.typed ALTER COLUMN label TYPE inc_a.label_t`, 1, true},
	{"rename table", `ALTER TABLE inc_a.refunds RENAME TO returns`, 1, false},
	{"set schema", `CREATE SCHEMA inc_b; ALTER TABLE inc_a.returns SET SCHEMA inc_b`,
		1, false},
	{"rename schema", `ALTER SCHEMA inc_b RENAME TO inc_c`, 0, false},
	{"drop table", `DROP TABLE inc_a.orders`, 0, false},
	{"rename a type to varchar", `ALTER DOMAIN inc_a.label_t RENAME TO "varchar"`,
		allTables, false},
	{"partitioned table and partition", `CREATE TABLE inc_a.parted (k int, shard_id text)
		PARTITION BY RANGE (k);
		CREATE TABLE inc_a.parted_1 PARTITION OF inc_a.parted FOR VALUES FROM (0) TO (10)`,
		2, false},
	{"table in an excluded schema", `CREATE TABLE sage.inc_probe (a text, b text, c_id text)`,
		0, false},
	{"drop schema", `DROP SCHEMA inc_c CASCADE`, 0, false},
}

func TestStructuralIncremental_EqualsAFullScanAfterEveryDDL(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, incrementalFixture)
	settledWatermark(t, dsn)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, dsn, func() time.Time { return clock })
	assertReference(t, dsn, "first pass", structuralAnswer(t, detector))
	if tr := tracePasses(t, rec); tr.passes != 1 || tr.reaggregated() != 3 {
		t.Fatalf("first pass %+v, want one full pass over the 3 tables", tr)
	}
	for _, step := range incrementalSteps {
		ddl(t, dsn, step.sql)
		clock = clock.Add(structuralMinInterval)
		assertReference(t, dsn, step.name, structuralAnswer(t, detector))
		tr := tracePasses(t, rec)
		want := step.reaggregated
		if want == allTables {
			want = userTables(t, dsn)
		}
		if tr.passes != 1 || tr.reaggregated() != want ||
			(tr.versions == 1) != step.verified || tr.versions > 1 {
			t.Fatalf("%s: pass %+v, want 1 pass aggregating %d tables, verified %v",
				step.name, tr, want, step.verified)
		}
		if want == 0 && len(tr.aggregated) != 0 {
			t.Fatalf("%s: an empty column aggregation was sent: %+v", step.name, tr)
		}
	}
}

// An unchanged catalog does no per-column work: the hourly floor checks
// the column versions and aggregates nothing; temporary-table churn moves
// the counters but neither checks nor aggregates columns; freezing the
// catalogs changes no row version.
func TestStructuralIncremental_UnchangedCatalogDoesNoColumnWork(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, incrementalFixture)
	settledWatermark(t, dsn)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, dsn, func() time.Time { return clock })
	first := structuralAnswer(t, detector)
	tracePasses(t, rec)
	steps := []struct {
		name     string
		advance  time.Duration
		prepare  func()
		versions int
	}{
		{"hourly floor", structuralMaxAge, func() {}, 1},
		{"temporary table churn", structuralMinInterval, func() {
			ddl(t, dsn, `CREATE TEMP TABLE churn (a text, b text, c_id text);
				DROP TABLE churn`)
		}, 0},
		{"catalogs frozen", structuralMaxAge, func() {
			execAndClose(t, dsn,
				`VACUUM (FREEZE) pg_catalog.pg_class, pg_catalog.pg_attribute`)
		}, 1},
	}
	for _, s := range steps {
		s.prepare()
		clock = clock.Add(s.advance)
		got := structuralAnswer(t, detector)
		tr := tracePasses(t, rec)
		if tr.passes != 1 || tr.versions != s.versions || len(tr.aggregated) != 0 {
			t.Fatalf("%s: pass %+v, want 1 pass, %d version checks, no aggregation",
				s.name, tr, s.versions)
		}
		if strings.Join(got, "\n") != strings.Join(first, "\n") {
			t.Fatalf("%s: answer changed without DDL:\n got %q\nwant %q", s.name, got, first)
		}
		assertReference(t, dsn, s.name, got)
	}
}

// A rewritten pg_attribute moves every column row (VACUUM FULL); the
// answer still equals a full scan. A day after the last full pass every
// table is aggregated again whatever the counters say.
func TestStructuralIncremental_RewrittenCatalogAndDailyFullPass(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, incrementalFixture)
	settledWatermark(t, dsn)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	detector, rec := recordingDetector(t, dsn, func() time.Time { return clock })
	structuralAnswer(t, detector)
	tracePasses(t, rec)
	execAndClose(t, dsn, `VACUUM FULL pg_catalog.pg_attribute`)
	clock = clock.Add(structuralMaxAge)
	assertReference(t, dsn, "after VACUUM FULL", structuralAnswer(t, detector))
	if tr := tracePasses(t, rec); tr.passes != 1 || tr.versions != 1 {
		t.Fatalf("after VACUUM FULL: pass %+v, want one verified pass", tr)
	}
	clock = clock.Add(structuralFullInterval)
	assertReference(t, dsn, "daily", structuralAnswer(t, detector))
	if tr := tracePasses(t, rec); tr.passes != 1 || tr.versions != 0 ||
		tr.reaggregated() != 3 {
		t.Fatalf("a day later: pass %+v, want a full pass over the 3 tables", tr)
	}
}

// Temporary tables are another session's scratch space: never reported,
// although the v2.3.1 scan (no persistence filter) reported them.
func TestStructuralIncremental_TemporaryTablesAreExcluded(t *testing.T) {
	dsn := cadenceDatabase(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`CREATE TEMP TABLE scratch (a text, b text, c_id text)`); err != nil {
		t.Fatalf("temp table: %v", err)
	}
	unfiltered := strings.Replace(structuralReferenceSQL,
		"AND tbl.relpersistence <> 't'", "", 1)
	if rows := rowsOf(t, dsn, unfiltered); len(rows) != 2 {
		t.Fatalf("without the filter the scan reports %q, want the temp table's 2 rows",
			rows)
	}
	detector, _ := recordingDetector(t, dsn, nil)
	if got := structuralAnswer(t, detector); len(got) != 0 {
		t.Fatalf("temporary table reported: %q", got)
	}
}

// A table an extension owns (hint_plan.hints, PostGIS's spatial_ref_sys)
// is the extension's to define: never reported, and its columns never
// aggregated. plpgsql is installed in every database.
func TestStructuralIncremental_ExtensionTablesAreExcluded(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, `CREATE TABLE ext_owned (a text, b text, c_id text);
		ALTER EXTENSION plpgsql ADD TABLE ext_owned`)
	unfiltered := strings.Replace(structuralReferenceSQL, extensionMemberFilter, "", 1)
	if rows := rowsOf(t, dsn, unfiltered); len(rows) != 2 {
		t.Fatalf("without the filter the scan reports %q, want the extension table's 2 rows",
			rows)
	}
	detector, rec := recordingDetector(t, dsn, nil)
	if got := structuralAnswer(t, detector); len(got) != 0 {
		t.Fatalf("extension table reported: %q", got)
	}
	if tr := tracePasses(t, rec); tr.passes != 1 || len(tr.aggregated) != 0 {
		t.Fatalf("extension table: %+v, want one pass and no column aggregation", tr)
	}
}

// A catalog without user tables answers nothing and aggregates nothing.
func TestStructuralIncremental_NoUserTables(t *testing.T) {
	dsn := cadenceDatabase(t)
	detector, rec := recordingDetector(t, dsn, nil)
	if got := structuralAnswer(t, detector); len(got) != 0 {
		t.Fatalf("empty catalog answered %q", got)
	}
	if tr := tracePasses(t, rec); tr.passes != 1 || len(tr.aggregated) != 0 {
		t.Fatalf("empty catalog: %+v, want one pass and no column aggregation", tr)
	}
}

// A detector without a cache runs a full pass on every call.
func TestStructuralIncremental_UncachedDetectorPassesInFull(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, incrementalFixture)
	detector, rec := recordingDetector(t, dsn, nil)
	uncached := postgresSchemaDetector{pool: detector.pool}
	for i := 0; i < 2; i++ {
		assertReference(t, dsn, "uncached", structuralAnswer(t, uncached))
		if tr := tracePasses(t, rec); tr.passes != 1 || tr.reaggregated() != 3 {
			t.Fatalf("uncached call %d: %+v, want a full pass over the 3 tables", i, tr)
		}
	}
}

// Callers that arrive together while a pass is due share one pass and
// one answer.
func TestStructuralIncremental_ConcurrentCallersShareOnePass(t *testing.T) {
	dsn := cadenceDatabase(t)
	execAndClose(t, dsn, incrementalFixture)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	detector, rec := recordingDetector(t, dsn, now)
	structuralAnswer(t, detector)
	tracePasses(t, rec)
	mu.Lock()
	clock = clock.Add(structuralMaxAge)
	mu.Unlock()
	answers := make([][]schemaguard.Invariant, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i], errs[i] = detector.detectStructuralPathologies(context.Background())
		}(i)
	}
	wg.Wait()
	want := rowsOf(t, dsn, structuralReferenceSQL)
	for i := range answers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		got := []string{}
		for _, item := range answers[i] {
			got = append(got, invariantRow(item))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("caller %d: %q, want %q", i, got, want)
		}
	}
	if tr := tracePasses(t, rec); tr.passes != 1 {
		t.Fatalf("%d passes for 8 concurrent callers, want 1", tr.passes)
	}
}
