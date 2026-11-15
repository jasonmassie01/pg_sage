package histstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01b: the derived rollup tables (the daily tier and the hourly
// coverage) move with a database's history, in (day, queryid) and hour
// order, resuming from their progress row; the hourly tier is not copied
// (it is rebuilt from the snapshots); status, cleanup and the history
// size include them.

// seedRollup writes days x queryids daily rows and days x 24 coverage rows
// of database id (0: monitored) from start.
func seedRollup(t *testing.T, pool *pgxpool.Pool, id int, start time.Time, days,
	queryids int) {
	t.Helper()
	var dbid any
	if id > 0 {
		dbid = id
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_daily (database_id, queryid,
		day, calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
		temp_blks_written, wal_bytes, hours, epoch_breaks, plan_hashes, last_plan_hash)
		SELECT $1, q, ($2::timestamptz + make_interval(days => d))::date, q * 10 + d,
			q * 1.5, q, q, q, 0, 0, NULL, 24, 0, 1, 'p' || q
		FROM generate_series(0, $3 - 1) d, generate_series(1, $4) q`,
		dbid, start, days, queryids); err != nil {
		t.Fatalf("seed daily rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage (database_id, hour,
		db_exec_ms, sampled_exec_ms, sampled_queryids, epoch_breaks, dealloc_delta,
		snapshots, epoch_source)
		SELECT $1, $2::timestamptz + make_interval(hours => h), 100, 87, $4, 0, 0, 12,
			'recorded' FROM generate_series(0, $3 * 24 - 1) h`,
		dbid, start, days, queryids); err != nil {
		t.Fatalf("seed coverage rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (database_id, queryid,
		hour, calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
		temp_blks_written, epoch_break, pairs) VALUES ($1, 1, $2, 1, 1, 1, 1, 1, 0, 0,
		false, 1)`, dbid, start); err != nil {
		t.Fatalf("seed an hourly row: %v", err)
	}
}

// rollupRows lists a table's rows of one database as text, in key order.
func rollupRows(t *testing.T, pool *pgxpool.Pool, table, where string) []string {
	t.Helper()
	order := "day, queryid"
	if table == "query_rollup_coverage" {
		order = "hour"
	}
	rows, err := pool.Query(context.Background(), `SELECT (to_jsonb(r) - 'database_id' -
		'built_at')::text FROM sage.`+table+` r `+where+` ORDER BY `+order)
	if err != nil {
		t.Fatalf("read sage.%s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func rollupStart() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -10)
}

func TestMigrateMovesTheDerivedRollupTables(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollupTables(t, p)
	seedRollup(t, p.Monitored, 0, rollupStart(), 3, 5)
	seedRollup(t, p.Meta, histfixture.OtherDatabaseID, rollupStart(), 3, 5)
	noise := rollupRows(t, p.Meta, "query_rollup_daily", "WHERE database_id = 8")
	rep := migrate(t, histstore.NewMonitored(p.Monitored), p.MetaStore(t),
		histstore.MigrateOptions{BatchRows: 4})
	for name, n := range map[string]int64{"query_rollup_daily": 15,
		"query_rollup_coverage": 72} {
		tr := table(t, rep, name)
		if !tr.Complete || tr.Copied != n {
			t.Fatalf("%s: %+v, want %d copied and complete", name, tr, n)
		}
		equal(t, "sage."+name+" in the store", rollupRows(t, p.Monitored, name, ""),
			rollupRows(t, p.Meta, name, scoped))
	}
	for _, tr := range rep.Tables {
		if tr.Table == "query_rollup_hourly" {
			t.Fatalf("the hourly tier is rebuilt, never copied: %+v", tr)
		}
	}
	if rep.HourlyRebuilt {
		t.Fatal("no Rebuild hook was given: the report must not claim a rebuild")
	}
	equal(t, "the other database's daily rows", noise,
		rollupRows(t, p.Meta, "query_rollup_daily", "WHERE database_id = 8"))
	if order := rep.Tables[0].Table + "," + rep.Tables[1].Table; order !=
		"snapshots,query_store" {
		t.Fatalf("the history tables still copy first, in their order: %s", order)
	}
}

// A copy that fails in the middle of sage.query_rollup_daily resumes from
// its progress row and copies no row twice.
func TestMigrateRollupResumesAfterAFailedBatch(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollupTables(t, p)
	seedRollup(t, p.Monitored, 0, rollupStart(), 4, 5) // 20 daily rows
	ctx := context.Background()
	if _, err := p.Meta.Exec(ctx, `CREATE OR REPLACE FUNCTION sage.fail_rollup_copy()
		RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.queryid = 3 AND NEW.day = (now() at time zone 'utc')::date - 8 THEN
			RAISE EXCEPTION 'injected copy failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_rollup_copy BEFORE INSERT ON sage.query_rollup_daily
		FOR EACH ROW EXECUTE FUNCTION sage.fail_rollup_copy()`); err != nil {
		t.Fatal(err)
	}
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	_, err := histstore.Migrate(ctx, src, dst, histstore.MigrateOptions{BatchRows: 4})
	if err == nil || !strings.Contains(err.Error(), "query_rollup_daily") {
		t.Fatalf("the injected failure must stop the copy naming the table, got %v", err)
	}
	var copied int64
	if err := p.Meta.QueryRow(ctx, `SELECT copied FROM sage.history_migration
		WHERE database_id = 7 AND table_name = 'query_rollup_daily'`).Scan(&copied); err != nil {
		t.Fatalf("the committed batches must leave a progress row: %v", err)
	}
	if copied != 8 { // day 0 and day 1 (2 batches), the failure is on day 2
		t.Fatalf("progress says %d copied before the failure, want 8", copied)
	}
	if _, err := p.Meta.Exec(ctx, `DROP TRIGGER fail_rollup_copy ON sage.query_rollup_daily;
		DROP FUNCTION sage.fail_rollup_copy()`); err != nil {
		t.Fatal(err)
	}
	rep := migrate(t, src, dst, histstore.MigrateOptions{BatchRows: 4})
	tr := table(t, rep, "query_rollup_daily")
	if !tr.Complete || tr.Copied != 12 {
		t.Fatalf("the resumed run %+v, want the 12 rows not yet copied", tr)
	}
	equal(t, "daily rows after the resume", rollupRows(t, p.Monitored,
		"query_rollup_daily", ""), rollupRows(t, p.Meta, "query_rollup_daily", scoped))
	again := migrate(t, src, dst, histstore.MigrateOptions{})
	if tr := table(t, again, "query_rollup_daily"); tr.Copied != 0 || !tr.Complete {
		t.Fatalf("a re-run must copy nothing: %+v", tr)
	}
}

func TestStatusCleanupAndSizeIncludeTheRollup(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	truncateRollupTables(t, p)
	ctx := context.Background()
	seedRollup(t, p.Monitored, 0, rollupStart(), 2, 3)
	src, dst := histstore.NewMonitored(p.Monitored), p.MetaStore(t)
	before, err := dst.HistoryBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := histstore.Status(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for _, s := range st {
		pending[s.Table] = s.Pending
	}
	if !pending["query_rollup_daily"] || !pending["query_rollup_coverage"] {
		t.Fatalf("status %+v must list the derived tables as pending", st)
	}
	if _, err := histstore.Cleanup(ctx, src, dst); err == nil {
		t.Fatal("cleanup must refuse while the derived rows are not copied")
	}
	migrate(t, src, dst, histstore.MigrateOptions{})
	clean, err := histstore.Cleanup(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Removed["query_rollup_daily"] != 6 || clean.Removed["query_rollup_coverage"] !=
		48 || clean.Removed["query_rollup_hourly"] != 1 {
		t.Fatalf("cleanup removed %v, want 6 daily, 48 coverage, 1 hourly", clean.Removed)
	}
	if _, err := p.Meta.Exec(ctx, "ANALYZE sage.query_rollup_daily, "+
		"sage.query_rollup_coverage"); err != nil {
		t.Fatal(err)
	}
	after, err := dst.HistoryBytes(ctx)
	if err != nil || after <= before {
		t.Fatalf("history bytes %d -> %d (err %v): the rollup rows must count", before,
			after, err)
	}
}

func truncateRollupTables(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	for _, pool := range []*pgxpool.Pool{p.Monitored, p.Meta} {
		if _, err := pool.Exec(context.Background(), `TRUNCATE sage.query_rollup_hourly,
			sage.query_rollup_daily, sage.query_rollup_coverage`); err != nil {
			t.Fatal(err)
		}
	}
}
