package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Perf storage phase (reviews/2026-10-03-perf-storage-report.md): pg_sage's
// own history tables are partitioned by UTC day so retention drops days
// instead of deleting rows, hot-update tables keep their updates HOT, and
// small hot tables carry their own autovacuum settings.

func relkindOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rel string) string {
	t.Helper()
	var k string
	if err := pool.QueryRow(ctx, `SELECT relkind::text FROM pg_class
		WHERE oid = to_regclass($1)`, rel).Scan(&k); err != nil {
		t.Fatalf("relkind of %s: %v", rel, err)
	}
	return k
}

func indexDefs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT indexname || ' ' || indexdef FROM pg_indexes
		WHERE schemaname = 'sage' AND tablename = $1 ORDER BY indexname`, table)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return defs
}

func TestStorageMigration_HistoryTablesArePartitionedByDay(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"query_store", "snapshots"} {
		if k := relkindOf(t, ctx, pool, "sage."+table); k != "p" {
			t.Fatalf("sage.%s relkind = %q, want partitioned", table, k)
		}
		var history, deflt, days int
		if err := pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE c.relname = $1 || '_history'),
			count(*) FILTER (WHERE c.relname = $1 || '_default'),
			count(*) FILTER (WHERE c.relname ~ ('^' || $1 || '_p[0-9]{8}$'))
			FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = to_regclass('sage.' || $1)`, table).
			Scan(&history, &deflt, &days); err != nil {
			t.Fatal(err)
		}
		if history != 1 || deflt != 1 || days < 2 {
			t.Fatalf("sage.%s partitions: history %d, default %d, days %d; want 1, 1, >= 2",
				table, history, deflt, days)
		}
	}
	qs := strings.Join(indexDefs(t, ctx, pool, "query_store"), "\n")
	for _, want := range []string{"idx_query_store_qid_time ", "idx_query_store_time "} {
		if !strings.Contains(qs, want) {
			t.Fatalf("query_store indexes lack %s:\n%s", want, qs)
		}
	}
	// query_store_pkey (108 MB on lifeos) was never scanned: no reader looks
	// a sample up by id.
	if strings.Contains(qs, "pkey") {
		t.Fatalf("query_store still has a primary key:\n%s", qs)
	}
	snap := strings.Join(indexDefs(t, ctx, pool, "snapshots"), "\n")
	for _, want := range []string{"snapshots_pkey ", "(id, collected_at)", "idx_snapshots_base ",
		"idx_snapshots_category ", "idx_snapshots_time "} {
		if !strings.Contains(snap, want) {
			t.Fatalf("snapshots indexes lack %s:\n%s", want, snap)
		}
	}
}

// An install upgraded from v1.8.2 has plain history tables full of rows:
// bootstrap converts them in place and keeps every row and id.
func TestStorageMigration_ConvertsAPopulatedPlainQueryStore(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	serializeAcrossPackages(t, ctx, pool)
	for _, stmt := range []string{
		`DROP TABLE sage.query_store CASCADE`,
		ddlQueryStore, ddlQueryStoreStatsEpoch,
		`ALTER TABLE sage.query_store ADD COLUMN IF NOT EXISTS plan_hash text`,
		`INSERT INTO sage.query_store (captured_at, queryid, calls, total_exec_time,
		    mean_exec_time) SELECT now() - g * interval '1 hour', g % 7, g, g, 1
		  FROM generate_series(1, 500) g`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("legacy setup %q: %v", stmt, err)
		}
	}
	var maxID int64
	if err := pool.QueryRow(ctx, `SELECT max(id) FROM sage.query_store`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	bootstrapWithRetry(t, ctx, pool)
	if k := relkindOf(t, ctx, pool, "sage.query_store"); k != "p" {
		t.Fatalf("relkind after bootstrap = %q", k)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.query_store_history`).
		Scan(&n); err != nil || n != 500 {
		t.Fatalf("history rows = %d, %v; want 500", n, err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.query_store (queryid, calls,
		total_exec_time, mean_exec_time) VALUES (1, 1, 1, 1) RETURNING id`).
		Scan(&id); err != nil || id <= maxID {
		t.Fatalf("insert after conversion: id %d (max before %d), %v", id, maxID, err)
	}
}

// findings were 0% HOT on lifeos: the analyzer refreshes last_seen on every
// open finding each cycle and last_seen was a key of two indexes and the
// retention index. No findings index may reference it.
func TestStorageMigration_FindingsIndexesLeaveLastSeenOut(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	defs := indexDefs(t, ctx, pool, "findings")
	joined := strings.Join(defs, "\n")
	for _, refreshed := range []string{"last_seen", "occurrence_count", "detail", "title",
		"recommendation", "recommended_sql", "rollback_sql", "impact_score"} {
		for _, d := range defs {
			if strings.Contains(d, refreshed) && !strings.Contains(d, "last_seen_at") {
				t.Errorf("index on a column every refresh updates (%s): %s", refreshed, d)
			}
		}
	}
	for _, want := range []string{
		"idx_findings_status_severity ", "(status, severity)",
		"idx_findings_resolved_at ", "(resolved_at) WHERE (status = 'resolved'::text)",
		"idx_findings_schema_lint_open ",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("findings indexes lack %s:\n%s", want, joined)
		}
	}
}

// Retention now ages resolved findings by resolved_at; rows resolved before
// that column was always set take their last_seen.
func TestStorageMigration_BackfillsResolvedAt(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status, last_seen, resolved_at)
		VALUES ('storage_test', 'info', 'table', 'storage.backfill', 't', '{}', 'resolved',
		        now() - interval '200 days', NULL) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings WHERE id = $1`, id)
	})
	bootstrapWithRetry(t, ctx, pool)
	var gap float64
	if err := pool.QueryRow(ctx, `SELECT abs(extract(epoch FROM resolved_at - last_seen))
		FROM sage.findings WHERE id = $1`, id).Scan(&gap); err != nil || gap != 0 {
		t.Fatalf("resolved_at - last_seen = %v s, %v; want backfilled", gap, err)
	}
}

func TestStorageMigration_SetsStorageParameters(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	want := map[string][]string{
		"findings":              {"fillfactor=80"},
		"incidents":             {"fillfactor=80", "autovacuum_vacuum_threshold=10"},
		"recommendation":        {"fillfactor=90"},
		"decision":              {"fillfactor=90"},
		"sre_change_feed_state": {"fillfactor=50", "autovacuum_vacuum_threshold=10"},
		"sre_service_slos":      {"fillfactor=50", "autovacuum_vacuum_threshold=10"},
		"sre_investigations":    {"fillfactor=70", "autovacuum_vacuum_threshold=10"},
	}
	for table, opts := range want {
		var reloptions []string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_class
			WHERE oid = to_regclass($1)`, "sage."+table).Scan(&reloptions); err != nil {
			t.Fatalf("reloptions of %s: %v", table, err)
		}
		got := strings.Join(reloptions, ",")
		for _, o := range opts {
			if !strings.Contains(got, o) {
				t.Errorf("sage.%s reloptions = %s, want %s", table, got, o)
			}
		}
	}
}

// A second bootstrap changes nothing: no index or partition is rebuilt.
func TestStorageMigration_SecondBootstrapIsANoOp(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT string_agg(c.relname || ':' || c.relfilenode,
			',' ORDER BY c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'sage' AND (c.relname LIKE 'idx_findings%'
			   OR c.relname LIKE 'query_store%' OR c.relname LIKE 'snapshots%'
			   OR c.relname LIKE 'idx_explain_results%')`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	bootstrapWithRetry(t, ctx, pool)
	if after := snapshot(); after != before {
		t.Fatalf("second bootstrap rebuilt relations:\n%s\n->\n%s", before, after)
	}
}

func TestStorageMigration_ExplainResultsExpiryIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	defs := strings.Join(indexDefs(t, ctx, pool, "explain_results"), "\n")
	if !strings.Contains(defs, "idx_explain_results_expires ") ||
		!strings.Contains(defs, "(expires_at)") {
		t.Fatalf("explain_results indexes:\n%s", defs)
	}
}
