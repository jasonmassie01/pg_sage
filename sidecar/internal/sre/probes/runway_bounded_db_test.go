package probes

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate offender 6 and measured.md M8 (v1.8.3): the runway
// trend regression sorted every sample of every series in the window
// (108 ms a call at 150k samples) to return at most 200 series, and the
// wraparound ranking computed the statistics view for every relation of
// the catalog (118 ms, ~78 times an hour on lifeos) to return 50. The
// autovacuum-cancellation count read every incident.

func bootstrappedTx(t *testing.T) (pgx.Tx, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx, pool, ctx
}

// legacyRunwayTrendsSQL is runway_trends v1 as shipped before v1.8.3: the
// reference for what every trend must be.
const legacyRunwayTrendsSQL = `
WITH s AS (
    SELECT r.kind, r.subject, r.epoch, r.sampled_at, r.value, r.counter, r.limit_value,
           EXTRACT(EPOCH FROM r.sampled_at)::float8 AS t
    FROM sage.runway_samples r
    WHERE r.sampled_at >= pg_catalog.now() - pg_catalog.make_interval(secs => $2)
), cur AS (
    SELECT DISTINCT ON (s.kind, s.subject) s.kind, s.subject, s.epoch,
           s.sampled_at AS last_at, s.value AS last_value, s.limit_value AS last_limit
    FROM s ORDER BY s.kind, s.subject, s.sampled_at DESC
)
SELECT c.kind, c.subject, count(*)::int8 AS samples, min(s.sampled_at) AS first_at,
       c.last_at, c.last_value, c.last_limit,
       pg_catalog.regr_slope(COALESCE(s.counter, s.value), s.t) AS rate_per_s,
       pg_catalog.regr_r2(COALESCE(s.counter, s.value), s.t) AS r2
FROM cur c
JOIN s ON s.kind = c.kind AND s.subject = c.subject AND s.epoch = c.epoch
GROUP BY c.kind, c.subject, c.last_at, c.last_value, c.last_limit
ORDER BY c.kind, c.subject
LIMIT $1`

// legacyWraparoundTablesSQL is wraparound_tables v1 as shipped before
// v1.8.3: the reference ranking.
var legacyWraparoundTablesSQL = `
WITH g AS (
    SELECT pg_catalog.current_setting('autovacuum_freeze_max_age')::int8 AS xid_max,
           pg_catalog.current_setting('autovacuum_multixact_freeze_max_age')::int8
               AS mxid_max
), t AS (
    SELECT c.oid, pg_catalog.age(c.relfrozenxid)::int8 AS xid_age,
           pg_catalog.mxid_age(c.relminmxid)::int8 AS mxid_age,
           LEAST(g.xid_max, COALESCE(o.xid_max, g.xid_max)) AS freeze_max_age,
           LEAST(g.mxid_max, COALESCE(o.mxid_max, g.mxid_max)) AS mxid_freeze_max_age,
           COALESCE(o.enabled, true) AS autovacuum_enabled
    FROM pg_catalog.pg_class c CROSS JOIN g
    LEFT JOIN LATERAL (
        SELECT max(CASE WHEN x.option_name = 'autovacuum_freeze_max_age'
                        THEN x.option_value::int8 END) AS xid_max,
               max(CASE WHEN x.option_name = 'autovacuum_multixact_freeze_max_age'
                        THEN x.option_value::int8 END) AS mxid_max,
               bool_and(CASE WHEN x.option_name = 'autovacuum_enabled'
                        THEN pg_catalog.lower(x.option_value) NOT IN
                             ('false', 'off', 'no', '0') END) AS enabled
        FROM pg_catalog.pg_options_to_table(c.reloptions) x
    ) o ON true
    WHERE c.relkind IN ('r', 'm', 't') AND c.relfrozenxid <> '0'::xid
)
SELECT ` + fmt.Sprintf(relationName, "t.oid") + ` AS relation,
       t.xid_age, t.mxid_age, t.freeze_max_age, t.mxid_freeze_max_age,
       t.autovacuum_enabled, s.n_dead_tup::int8 AS n_dead_tup,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
           - GREATEST(s.last_autovacuum, s.last_vacuum))::float8 AS last_vacuum_age_s,
       s.autovacuum_count::int8 AS autovacuum_count,
       EXISTS (SELECT 1 FROM pg_catalog.pg_stat_progress_vacuum p
               WHERE p.relid = t.oid AND p.datname = pg_catalog.current_database())
           AS vacuum_running
FROM t LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = t.oid
ORDER BY GREATEST(t.xid_age::float8 / NULLIF(t.freeze_max_age, 0),
                  t.mxid_age::float8 / NULLIF(t.mxid_freeze_max_age, 0)) DESC NULLS LAST,
         t.oid
LIMIT $1`

type trendRow struct {
	kind, subject       string
	samples             int64
	firstAt, lastAt     time.Time
	lastValue           float64
	lastLimit, rate, r2 *float64
}

func readTrends(t *testing.T, ctx context.Context, tx pgx.Tx, sql string) []trendRow {
	t.Helper()
	rows, err := tx.Query(ctx, sql, 201, 6*3600.0)
	if err != nil {
		t.Fatalf("trends: %v", err)
	}
	defer rows.Close()
	var out []trendRow
	for rows.Next() {
		var r trendRow
		if err := rows.Scan(&r.kind, &r.subject, &r.samples, &r.firstAt, &r.lastAt,
			&r.lastValue, &r.lastLimit, &r.rate, &r.r2); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func near(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) <= 1e-9*math.Max(1, math.Abs(*b))
}

func TestRunwayTrends_SameTrendsBoundedRead(t *testing.T) {
	tx, _, ctx := bootstrappedTx(t)
	// 300 series of 120 samples in the 6 h window plus 40 older ones;
	// every fifth series starts a new epoch in the window; every seventh
	// has no counter (value regression) and every eleventh no limit.
	if _, err := tx.Exec(ctx, `INSERT INTO sage.runway_samples (kind, subject, epoch,
		sampled_at, value, counter, limit_value)
		SELECT 'trend_' || (s % 3), 'subj_' || lpad(s::text, 4, '0'),
		       CASE WHEN s % 5 = 0 AND g > 110 THEN 'b' ELSE 'a' END,
		       now() - ((160 - g) * interval '3 minutes') - interval '1 second',
		       s * 10 + g * (1 + s % 4), CASE WHEN s % 7 <> 0 THEN s * 1000 + g * 3 END,
		       CASE WHEN s % 11 <> 0 THEN 1e9 END
		FROM generate_series(1, 300) s, generate_series(1, 160) g;
		ANALYZE sage.runway_samples`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := readTrends(t, ctx, tx, legacyRunwayTrendsSQL)
	before, err := testdb.XactScansOf(ctx, tx, "sage.runway_samples")
	if err != nil {
		t.Fatal(err)
	}
	got := readTrends(t, ctx, tx, runwayTrendsSQL)
	after, err := testdb.XactScansOf(ctx, tx, "sage.runway_samples")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) != 201 {
		t.Fatalf("trends = %d rows, want %d (the limit)", len(got), len(want))
	}
	var inWindow int64
	for i, w := range want {
		g := got[i]
		if g.kind != w.kind || g.subject != w.subject || g.samples != w.samples ||
			!g.firstAt.Equal(w.firstAt) || !g.lastAt.Equal(w.lastAt) ||
			g.lastValue != w.lastValue || !near(g.lastLimit, w.lastLimit) ||
			!near(g.rate, w.rate) || !near(g.r2, w.r2) {
			t.Fatalf("trend %d = %+v, want %+v", i, g, w)
		}
		inWindow += 120
	}
	d := after.Minus(before)
	if d.Seq != 0 || d.IndexFetch > inWindow+3*int64(len(want)) {
		t.Fatalf("201 trends read %+v; want at most their %d window samples plus a few "+
			"rows per series", d, inWindow)
	}
	plan, err := testdb.Explain(ctx, tx, "", runwayTrendsSQL, 201, 6*3600.0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Has(func(n testdb.PlanNode) bool { return n.NodeType == "Sort" }) {
		t.Fatalf("runway trends sort the window's samples:\n%s", plan)
	}
}

// The wraparound ranking orders tables by age from pg_class alone and
// reads statistics, reloptions and vacuum progress only for the tables it
// returns: no statistics view over the whole catalog (it joins pg_index
// and aggregates every relation).
func TestWraparoundTables_RanksBeforeReadingStatistics(t *testing.T) {
	tx, _, ctx := bootstrappedTx(t)
	plan, err := testdb.Explain(ctx, tx, "ANALYZE", wraparoundTablesSQL, 51)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Has(func(n testdb.PlanNode) bool { return n.Relation == "pg_index" }) {
		t.Fatalf("wraparound ranking reads pg_index (the statistics view):\n%s", plan)
	}
	if plan.Has(func(n testdb.PlanNode) bool {
		return n.NodeType == "Aggregate" && n.ActualRows*n.ActualLoops > 51
	}) {
		t.Fatalf("wraparound ranking aggregates more relations than it returns:\n%s", plan)
	}
}

// The ranking is the one v1 returned: same tables, same ages, same
// limits, same flags.
func TestWraparoundTables_SameRankingAsV1(t *testing.T) {
	tx, _, ctx := bootstrappedTx(t)
	read := func(sql string) []map[string]any {
		rows, err := tx.Query(ctx, sql, 51)
		if err != nil {
			t.Fatalf("wraparound: %v", err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out {
			delete(r, "last_vacuum_age_s") // measured against clock_timestamp()
		}
		return out
	}
	want, got := read(legacyWraparoundTablesSQL), read(wraparoundTablesSQL)
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Fatalf("row %d %s = %v, want %v", i, k, got[i][k], v)
			}
		}
	}
}

func TestAutovacuumCancellations_ReadOnlyCancellationIncidents(t *testing.T) {
	tx, _, ctx := bootstrappedTx(t)
	if _, err := tx.Exec(ctx, `INSERT INTO sage.incidents (severity, root_cause,
		signal_ids, source, detected_at, last_detected_at, resolved_at)
		SELECT 'warning', 'perf', ARRAY['lock_contention'], 'deterministic',
		       now() - g * interval '1 minute', now() - g * interval '1 minute', now()
		FROM generate_series(1, 6000) g;
		INSERT INTO sage.incidents (severity, root_cause, signal_ids, source,
		  detected_at, last_detected_at)
		VALUES ('warning', 'perf', ARRAY['log_autovacuum_cancel'], 'log_deterministic',
		        now() - interval '2 days', now() - interval '5 minutes');
		ANALYZE sage.incidents`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := testdb.XactScansOf(ctx, tx, "sage.incidents")
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := tx.QueryRow(ctx, autovacuumCancellationsSQL, 2, 3600.0).Scan(&n); err != nil {
		t.Fatal(err)
	}
	after, err := testdb.XactScansOf(ctx, tx, "sage.incidents")
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("cancellations = %d, want the one detected 5 minutes ago", n)
	}
	if d := after.Minus(before); d.Seq != 0 {
		t.Fatalf("counting cancellations scanned sage.incidents: %+v", d)
	}
}
