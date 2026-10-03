package runway

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate offender 7: on (re)start the monitor read every sample
// of the retention window (all of sage.runway_samples: every row
// qualifies, so a sequential scan was the planner's right choice for that
// statement) to find each series' newest point. It now reads one row per
// series (an index skip scan), whatever the window holds.

// legacyLoadLastSQL is the statement the skip scan replaced: the reference
// for what each series' last point is.
const legacyLoadLastSQL = `SELECT DISTINCT ON (kind, subject) kind, subject, epoch, counter
	FROM sage.runway_samples
	WHERE sampled_at > now() - make_interval(secs => $1)
	ORDER BY kind, subject, sampled_at DESC`

type lastRow struct {
	epoch   string
	counter *float64
}

func readLast(t *testing.T, ctx context.Context, tx pgx.Tx, sql string,
	retention float64) map[seriesKey]lastRow {
	t.Helper()
	rows, err := tx.Query(ctx, sql, retention)
	if err != nil {
		t.Fatalf("read last samples: %v", err)
	}
	defer rows.Close()
	out := map[seriesKey]lastRow{}
	for rows.Next() {
		var k seriesKey
		var r lastRow
		if err := rows.Scan(&k.kind, &k.subject, &r.epoch, &r.counter); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLoadLast_OneRowPerSeries(t *testing.T) {
	pool, ctx := livePool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// 40 series x 300 samples over 40 hours, the newest epoch of each
	// series in its last 10 samples; one stale series older than the window.
	if _, err := tx.Exec(ctx, `INSERT INTO sage.runway_samples (kind, subject, epoch,
		sampled_at, value, counter, limit_value)
		SELECT 'skipscan', 's' || s, CASE WHEN g > 290 THEN 'e2' ELSE 'e1' END,
		       now() - ((300 - g) * interval '8 minutes'), g,
		       CASE WHEN s % 2 = 0 THEN s * 1000 + g END, 1e9
		FROM generate_series(1, 40) s, generate_series(1, 300) g;
		INSERT INTO sage.runway_samples (kind, subject, epoch, sampled_at, value)
		VALUES ('skipscan', 'stale', 'e1', now() - interval '3 days', 1);
		ANALYZE sage.runway_samples`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const retention = 48 * 3600.0
	want := readLast(t, ctx, tx, legacyLoadLastSQL, retention)
	var series int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT DISTINCT kind, subject
		FROM sage.runway_samples) s`).Scan(&series); err != nil {
		t.Fatal(err)
	}
	before, err := testdb.XactScansOf(ctx, tx, "sage.runway_samples")
	if err != nil {
		t.Fatal(err)
	}
	got := readLast(t, ctx, tx, loadLastSQL, retention)
	after, err := testdb.XactScansOf(ctx, tx, "sage.runway_samples")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("series = %d, want %d", len(got), len(want))
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || g.epoch != w.epoch || (g.counter == nil) != (w.counter == nil) ||
			(g.counter != nil && *g.counter != *w.counter) {
			t.Fatalf("series %v = %+v (present %v), want %+v", k, g, ok, w)
		}
	}
	if _, ok := got[seriesKey{"skipscan", "stale"}]; ok {
		t.Fatal("a series with no sample in the window was loaded")
	}
	if g := got[seriesKey{"skipscan", "s2"}]; g.epoch != "e2" || g.counter == nil ||
		*g.counter != 2300 {
		t.Fatalf("series s2 = %+v, want epoch e2 counter 2300", g)
	}
	d := after.Minus(before)
	if d.Seq != 0 || d.IndexFetch > 3*series {
		t.Fatalf("loading the last points of %d series read %+v, want one row per series",
			series, d)
	}
}
