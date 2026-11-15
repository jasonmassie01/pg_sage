package querystore

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-01b on a real server, in both placements: the coverage report of
// the long history sums the hourly coverage rows of a window, says
// "unknown" with nothing to measure, and measures the rollup's bytes.

// coverageFix writes one coverage row.
type coverageFix struct {
	hour            time.Time
	db              *float64
	sampled         float64
	queryids, brk   int
	dealloc         *int64
	source          string
}

func insertCoverage(t *testing.T, db any, c coverageFix) {
	t.Helper()
	if c.source == "" {
		c.source = "recorded"
	}
	_, err := histstore.Resolve(db).Exec(context.Background(), `INSERT INTO
		sage.query_rollup_coverage (hour, db_exec_ms, sampled_exec_ms, sampled_queryids,
		epoch_breaks, dealloc_delta, snapshots, epoch_source{dbcol})
		VALUES ($1, $2, $3, $4, $5, $6, 12, $7{dbval})`, c.hour, c.db, c.sampled,
		c.queryids, c.brk, c.dealloc, c.source)
	if err != nil {
		t.Fatalf("insert coverage %+v: %v", c, err)
	}
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestRollupCoverage_EmptyIsUnknownInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		truncateAllRollup(t, p)
		to := time.Now().UTC()
		rep, err := RollupCoverage(ctx, p.Monitored, to.AddDate(0, 0, -28), to)
		if err != nil {
			t.Fatalf("%s: coverage of an empty rollup: %v", mode, err)
		}
		if rep.Status != RollupCoverageUnknown || rep.Hours != 0 ||
			rep.CoveredExecShare != nil || rep.OldestHour != nil || rep.OldestDay != nil ||
			rep.EpochBreaks != 0 || rep.HoursMissing != 0 {
			t.Fatalf("%s: empty report %+v, want unknown with zero hours", mode, rep)
		}
	}
}

// seedFourWeeks writes 28 days of coverage from start (672 hours) with a
// 5-hour gap, three hours with breaks, two counters-only hours and one
// hour of unknown statement time, plus a daily row.
func seedFourWeeks(t *testing.T, db any, start time.Time) {
	t.Helper()
	for h := range 28 * 24 {
		if h >= 100 && h < 105 {
			continue // the sidecar was down
		}
		c := coverageFix{hour: start.Add(time.Duration(h) * time.Hour), db: f64(100),
			sampled: 87, queryids: 20, dealloc: i64(1)}
		switch {
		case h == 10 || h == 200 || h == 300:
			c.brk = 20
		case h == 400:
			c.db = nil // queries step unavailable that hour
		case h == 500 || h == 501:
			c.source = "counters_only"
		}
		insertCoverage(t, db, c)
	}
	insertHourly(t, db, hourlyFix{qid: 1, hour: start.Add(time.Hour), calls: 1, exec: 1})
	if _, err := RollupDay(context.Background(), db, start); err != nil {
		t.Fatal(err)
	}
}

func TestRollupCoverage_SumsTheWindowInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	start := pastDay(30)
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		truncateAllRollup(t, p)
		seedFourWeeks(t, p.Monitored, start)
		if mode == histstore.ModeMeta {
			other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
			if err != nil {
				t.Fatal(err)
			}
			insertCoverage(t, other, coverageFix{hour: start, db: f64(1e9), sampled: 1,
				queryids: 1, brk: 1})
		}
		rep, err := RollupCoverage(ctx, p.Monitored, start, start.AddDate(0, 0, 28))
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if rep.Status != RollupCoverageOK || rep.Hours != 667 || rep.HoursMissing != 5 ||
			rep.EpochBreaks != 3 || rep.CountersOnlyHours != 2 ||
			rep.DeallocGrowth == nil || *rep.DeallocGrowth != 667 {
			t.Fatalf("%s: report %+v", mode, rep)
		}
		if rep.CoveredExecShare == nil || math.Abs(*rep.CoveredExecShare-0.87) > 1e-12 {
			t.Fatalf("%s: covered share %v, want 0.87 (the unknown hour left out)", mode,
				rep.CoveredExecShare)
		}
		if rep.OldestHour == nil || !rep.OldestHour.Equal(start) || rep.OldestDay == nil ||
			!rep.OldestDay.Equal(start) {
			t.Fatalf("%s: oldest hour %v day %v, want %v", mode, rep.OldestHour,
				rep.OldestDay, start)
		}
		if rep.RollupBytes <= 0 {
			t.Fatalf("%s: rollup bytes %d, want a measurement", mode, rep.RollupBytes)
		}
		// A window inside the history: only its hours, and no gap at the edges.
		part, err := RollupCoverage(ctx, p.Monitored, start.Add(200*time.Hour),
			start.Add(210*time.Hour))
		if err != nil || part.Hours != 10 || part.HoursMissing != 0 || part.EpochBreaks != 1 {
			t.Fatalf("%s: part window %+v (err %v), want 10 hours, 1 break", mode, part, err)
		}
	}
}

func TestRollupBytes_MonitoredIsTheTablesMetaIsTheShare(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		truncateAllRollup(t, p)
		seedFourWeeks(t, p.Monitored, pastDay(30))
		got, err := RollupBytes(ctx, p.Monitored)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		pool := p.Monitored
		if mode == histstore.ModeMeta {
			pool = p.Meta
		}
		var whole int64
		if err := pool.QueryRow(ctx, `SELECT pg_total_relation_size('sage.query_rollup_hourly')
			+ pg_total_relation_size('sage.query_rollup_daily')
			+ pg_total_relation_size('sage.query_rollup_coverage')`).Scan(&whole); err != nil {
			t.Fatal(err)
		}
		if mode == histstore.ModeMonitored && got != whole {
			t.Fatalf("monitored: rollup bytes %d, want the tables' %d", got, whole)
		}
		if mode == histstore.ModeMeta && (got <= 0 || got > whole) {
			t.Fatalf("meta: rollup bytes %d, want a share in (0, %d]", got, whole)
		}
	}
}
