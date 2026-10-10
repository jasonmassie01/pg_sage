package perfgate

import (
	"strings"
	"testing"
)

// Gate B's mean charges every pg_sage statement that is not exempt by its
// tag, however often it ran: a catalog statement that skipped a cycle (or
// ran once) is still a statement every deployment pays for when it runs.
// The generic "infrequent catalog" exemption (PR #155) let a statement
// that missed one cycle of six escape the gate; it is gone.

func structuralScan(calls int64, mean float64) Statement {
	return Statement{QueryID: 9, Calls: calls, MeanMs: mean, MaxMs: mean,
		TotalMs: mean * float64(calls),
		Query:   "WITH /* pg_sage */ tables AS (SELECT 1 FROM pg_catalog.pg_attribute att)"}
}

func TestMeanGateChargesCatalogStatementsHoweverOftenTheyRan(t *testing.T) {
	for _, calls := range []int64{1, 5, 6, 12} {
		p := steadyPhase() // 6 cycles
		p.Statements = []Statement{structuralScan(calls, 135)}
		got, err := Evaluate([]Phase{p}, DefaultBudgets())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Gate != GateStatementMean || got[0].Measured != 135 ||
			got[0].Budget != DefaultBudgets().StatementMeanMs {
			t.Fatalf("%d calls of an untagged 135 ms catalog scan: %+v, want gate B", calls, got)
		}
	}
}

// The report no longer states the removed rule.
func TestReportDropsTheInfrequentCatalogRule(t *testing.T) {
	md := RenderMarkdown(SmallScale(), DefaultBudgets(), []Phase{steadyPhase()}, nil)
	if strings.Contains(md, "less than once per cycle") {
		t.Fatalf("report still states the infrequent catalog rule:\n%s", md)
	}
}

// Real pg_sage statements. A statement that reads a sage table is pg_sage
// history, judged by the mean like any other: pg_catalog function calls
// in it (now(), make_interval(), regr_slope()) do not make it a catalog
// read. A system function read as a row source (FROM pg_ls_waldir()) is
// a catalog read: like a catalog relation, its cost follows the size of
// the system it lists, and gate D's max holds it. Catalog reads stay
// catalog reads, whatever a comment or a string literal in them says.
var catalogClassification = []struct {
	name    string
	query   string
	catalog bool
}{
	{"sre:runway_trends v2", `/* pg_sage sre:runway_trends v2 */
WITH RECURSIVE series AS (
    (SELECT r.kind, r.subject FROM sage.runway_samples r
     ORDER BY r.kind, r.subject LIMIT 1)
    UNION ALL
    SELECT n.kind, n.subject FROM series s
    CROSS JOIN LATERAL (
        SELECT r.kind, r.subject FROM sage.runway_samples r
        WHERE (r.kind, r.subject) > (s.kind, s.subject)
        ORDER BY r.kind, r.subject LIMIT 1) n
)
SELECT s.kind, s.subject, a.samples, a.rate_per_s
FROM series s
CROSS JOIN LATERAL (
    SELECT count(*)::int8 AS samples,
           pg_catalog.regr_slope(COALESCE(r.counter, r.value),
               EXTRACT(EPOCH FROM r.sampled_at)::float8) AS rate_per_s
    FROM sage.runway_samples r
    WHERE r.kind = s.kind AND r.subject = s.subject
      AND r.sampled_at >= pg_catalog.now() - pg_catalog.make_interval(secs => $2)) a
LIMIT $1`, false},
	{"sre:autovacuum_cancellations v1", `/* pg_sage sre:autovacuum_cancellations v1 */
SELECT count(*)::int8 AS cancel_incidents
FROM sage.incidents i
WHERE 'log_autovacuum_cancel' = ANY (i.signal_ids)
  AND i.last_detected_at > pg_catalog.now()
      - pg_catalog.make_interval(secs => $2)
LIMIT $1`, false},
	{"sre:plan_regressions v1", `/* pg_sage sre:plan_regressions v1 */
WITH s AS (
    SELECT q.id, q.queryid, q.captured_at, q.plan_hash,
           (q.calls - lag(q.calls) OVER w)::float8 AS d_calls
    FROM sage.query_store q
    WHERE q.database_name = $4 AND q.captured_at >= pg_catalog.now()
              - pg_catalog.make_interval(secs => $2)
    WINDOW w AS (PARTITION BY q.queryid ORDER BY q.captured_at, q.id)
), span AS (
    SELECT queryid, (pg_catalog.array_agg(plan_hash ORDER BY captured_at DESC))[1]
               AS last_hash
    FROM s GROUP BY queryid
)
SELECT * FROM span LIMIT $1`, false},
	{"rca reconcile backfill identity", `/* pg_sage */
WITH b AS (
    SELECT id FROM sage.incidents
    WHERE resolved_at IS NULL AND identity_key IS NULL
      AND (database_name = $1 OR database_name IS NULL OR database_name = '')
    ORDER BY id LIMIT $2
)
UPDATE sage.incidents i SET database_name = $1, identity_key = pg_catalog.encode(
    pg_catalog.sha256(pg_catalog.convert_to(
    COALESCE((SELECT pg_catalog.string_agg(s, ',' ORDER BY s COLLATE "C")
              FROM pg_catalog.unnest(i.signal_ids) s), '') || E'\x1f' ||
    COALESCE(i.affected_objects[1], ''), 'UTF8')), 'hex')
FROM b WHERE i.id = b.id AND i.resolved_at IS NULL`, false},
	{"sre:slo_proxy captures", `/* pg_sage sre:slo_proxy */
SELECT DISTINCT q.captured_at FROM sage.query_store q
WHERE q.database_name = $1 AND q.captured_at > pg_catalog.now() - interval '30 minutes'
ORDER BY q.captured_at DESC LIMIT 2`, false},
	{"analyzer app-managed indexes", `/* pg_sage */
SELECT pg_catalog.lower(m[3]), al.rollback_sql, al.executed_at,
       pg_catalog.pg_get_indexdef(pg_catalog.to_regclass(m[3]))
FROM sage.action_log al,
     LATERAL pg_catalog.regexp_match(al.sql_executed,
         '^\s*DROP\s+INDEX\s+(CONCURRENTLY\s+)?(IF\s+EXISTS\s+)?([^\s;]+)', 'i') m
WHERE al.action_type = 'drop_index' AND al.rollback_sql IS NOT NULL
  AND al.executed_at > pg_catalog.now() - interval '180 days'`, false},
	{"sage read joined to a catalog", `/* pg_sage */ SELECT f.id FROM sage.findings f
JOIN pg_catalog.pg_class c ON c.relname = f.object_identifier`, false},
	{"quoted sage schema", `SELECT 1 FROM "sage".findings JOIN pg_class c ON true`, false},
	{"upper-case sage schema", `SELECT 1 FROM SAGE.findings, pg_stat_user_tables`, false},
	{"pg_catalog.now() alone", `/* pg_sage */ SELECT pg_catalog.now()`, false},
	{"pg_catalog function with space", `SELECT pg_catalog.current_setting ('work_mem')`, false},
	{"statistics reset call", `SELECT pg_stat_statements_reset()`, false},
	{"pg_catalog row source that reads no system state",
		`SELECT x FROM pg_catalog.unnest($1::int8[]) x`, false},

	{"sre:cluster_database_size v1", `/* pg_sage sre:cluster_database_size v1 */
SELECT (SELECT sum(pg_catalog.pg_database_size(d.oid))::int8
        FROM pg_catalog.pg_database d
        WHERE d.datallowconn AND pg_catalog.has_database_privilege(d.oid, 'CONNECT'))
           AS database_bytes
LIMIT $1`, true},
	{"schema guard structural scan", `/* pg_sage schema_guard:structural v1 */
WITH tables AS (
    SELECT ns.nspname AS schema_name, tbl.relname AS table_name, count(*) AS column_count
    FROM pg_class tbl
    JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
    JOIN pg_attribute att ON att.attrelid=tbl.oid AND att.attnum>0 AND NOT att.attisdropped
    WHERE tbl.relkind IN ('r','p')
      AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')
    GROUP BY tbl.oid, ns.nspname, tbl.relname
)
SELECT schema_name, table_name FROM tables WHERE column_count>=3`, true},
	{"missing FK index scan", `/* pg_sage */
SELECT ns.nspname, tbl.relname, con.conname
FROM pg_constraint con
JOIN pg_class tbl ON tbl.oid=con.conrelid
JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
WHERE con.contype='f'
  AND NOT EXISTS (SELECT 1 FROM pg_index idx WHERE idx.indrelid=con.conrelid)`, true},
	{"qualified catalog relation", `SELECT 1 FROM pg_catalog.pg_namespace n`, true},
	{"sre:wal_directory v1", `/* pg_sage sre:wal_directory v1 */
SELECT (SELECT COALESCE(sum(w.size), 0)::int8 FROM pg_catalog.pg_ls_waldir() w)
           AS wal_dir_bytes,
       (SELECT count(*)::int8 FROM pg_catalog.pg_ls_waldir() w) AS wal_files,
       (SELECT count(*)::int8 FROM pg_catalog.pg_ls_archive_statusdir() a
        WHERE a.name LIKE '%.ready') AS archive_ready_files
LIMIT $1`, true},
	{"sre:wal_runway v2", `/* pg_sage sre:wal_runway v2 */
SELECT pg_catalog.pg_is_in_recovery() AS in_recovery,
       (SELECT c.system_identifier::text FROM pg_catalog.pg_control_system() c)
           AS system_identifier,
       current_user::text AS role_name
LIMIT $1`, true},
	{"unqualified statistics function in FROM",
		`SELECT a.pid FROM pg_stat_get_activity(NULL) a`, true},
	{"system function joined LATERAL", `SELECT f FROM (VALUES (1)) v
CROSS JOIN LATERAL pg_catalog.pg_ls_dir('pg_wal') f`, true},
	{"system function in FROM in upper case",
		`SELECT count(*) FROM PG_CATALOG.PG_LS_WALDIR()`, true},
	{"statistics view", `/* pg_sage */ SELECT * FROM pg_stat_user_tables`, true},
	{"information schema", `select * from information_schema.columns`, true},
	{"catalog read mentioning sage in a comment",
		"/* pg_sage reads sage.findings later */ SELECT relname FROM pg_class", true},
	{"catalog read mentioning sage in a line comment",
		"SELECT relname FROM pg_class -- not sage.findings\nWHERE relkind = 'r'", true},
	{"catalog read with a sage name in a literal",
		`SELECT c.oid FROM pg_class c WHERE c.oid = 'sage.findings'::regclass`, true},
	{"catalog read with a quoted quote in a literal",
		`SELECT 1 FROM pg_class WHERE relname = 'it''s sage.x'`, true},
	{"pg_sage tag is not the sage schema",
		`/* pg_sage sre:x v1 */ SELECT pg_sage.f FROM pg_class pg_sage`, true},

	{"empty", "", false},
	{"plain sage read", `SELECT * FROM sage.findings`, false},
	{"sizing function", `SELECT pg_total_relation_size($1)`, false},
}

func TestIsCatalogQueryClassifiesRealStatements(t *testing.T) {
	for _, c := range catalogClassification {
		if got := IsCatalogQuery(c.query); got != c.catalog {
			t.Errorf("%s: IsCatalogQuery = %t, want %t\n%s", c.name, got, c.catalog, c.query)
		}
	}
}

// A sage history statement over the catalog max is charged by gate B's
// mean (steady phase), never by gate D.
func TestSageHistoryStatementIsNotChargedAsCatalog(t *testing.T) {
	p := steadyPhase()
	p.Statements = []Statement{{QueryID: 5, Calls: 6, MeanMs: 50, MaxMs: 900, TotalMs: 300,
		Query: catalogClassification[1].query}}
	got, err := Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a sage history read with a 900 ms max was charged: %+v", got)
	}
	p.Statements[0].MeanMs = 101
	got, err = Evaluate([]Phase{p}, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Gate != GateStatementMean {
		t.Fatalf("a 101 ms sage history read: %+v, want gate B", got)
	}
}
