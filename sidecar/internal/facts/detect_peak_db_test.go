package facts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// evid-03b on real PostgreSQL: SuggestPeakWindows reads 28 days of the
// hourly rollup (evid-01a) and its coverage (evid-01b). It suggests
// nothing with less than 28 days or a coverage status other than ok.
// No concurrent access tests: it only reads, in one bounded transaction.

// peakNow is the end of the 28-day window peakLoad fills.
var peakNow = peakFrom.Add(28 * 24 * time.Hour)

// seedRollup writes the coverage rows of every hour in [start, peakNow)
// (an epoch break in the hours breaks says) and one rollup row per hour of
// exec.
func seedRollup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, start time.Time,
	exec map[time.Time]float64, breaks func(time.Time) bool) {
	t.Helper()
	clean := func() {
		for _, table := range []string{"query_rollup_hourly", "query_rollup_coverage"} {
			_, _ = pool.Exec(context.Background(), "DELETE FROM sage."+table)
		}
	}
	clean()
	t.Cleanup(clean)
	var hours, broken []time.Time
	var ms []float64
	for at := start; at.Before(peakNow); at = at.Add(time.Hour) {
		if breaks != nil && breaks(at) {
			broken = append(broken, at)
		}
		if v, ok := exec[at]; ok {
			hours, ms = append(hours, at), append(ms, v)
		}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage (database_id, hour,
		db_exec_ms, sampled_exec_ms, sampled_queryids, epoch_breaks, snapshots, epoch_source)
		SELECT NULL, g, 200, 150, 3, CASE WHEN g = ANY($3) THEN 1 ELSE 0 END, 12, 'recorded'
		FROM generate_series($1::timestamptz, $2::timestamptz - interval '1 hour',
		                     interval '1 hour') g`, start, peakNow, broken); err != nil {
		t.Fatalf("seed coverage: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (database_id, queryid,
		hour, calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
		temp_blks_written, epoch_break, pairs)
		SELECT NULL, q, h, 10, e / 2, 10, 0, 0, 0, 0, false, 12
		FROM unnest($1::timestamptz[], $2::float8[]) AS x(h, e), (VALUES (1), (2)) AS v(q)`,
		hours, ms); err != nil {
		t.Fatalf("seed hourly rollup: %v", err)
	}
}

func mondayMorningPeaks() map[time.Time]float64 {
	return peakLoad(peakAt{time.Monday, 9, []int{0, 1, 2}},
		peakAt{time.Monday, 10, []int{0, 1, 3}}, peakAt{time.Monday, 11, []int{1, 2, 3}},
		peakAt{time.Tuesday, 14, []int{0, 1}})
}

func TestSuggestPeakWindowsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	seedRollup(t, ctx, pool, peakFrom, mondayMorningPeaks(), nil)
	got, err := SuggestPeakWindows(ctx, pool, peakNow.Add(25*time.Minute))
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if got.PeakWindows != "mon 09:00-12:00 UTC" || got.CoverageStatus != "ok" ||
		!got.From.Equal(peakFrom) || !got.To.Equal(peakNow) || len(got.Hours) != 9 {
		t.Fatalf("suggestion %+v", got)
	}
	// The two queryids of each hour are summed.
	if got.Hours[0].ExecMs != 10009 {
		t.Fatalf("first evidence hour %+v, want the hour's summed exec 10009", got.Hours[0])
	}
	if _, err := policy.ParseWindow(got.PeakWindows); err != nil {
		t.Fatalf("suggestion does not parse: %v", err)
	}
	brief := Proposal{Type: TypeOwnerBrief, Kind: KindDatabase, Subject: "*",
		Value: map[string]string{"owner": "Payments", "peak_windows": got.PeakWindows}}
	f, err := NewStore(pool).Declare(ctx, brief, "alice@example.com", "applied suggestion")
	if err != nil || f.Value["peak_windows"] != "mon 09:00-12:00 UTC" {
		t.Fatalf("apply the suggestion as the brief: %+v %v", f, err)
	}
}

func TestSuggestPeakWindowsNeedsTwentyEightDaysOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	seedRollup(t, ctx, pool, peakFrom.Add(time.Hour), mondayMorningPeaks(), nil)
	got, err := SuggestPeakWindows(ctx, pool, peakNow)
	if err != nil || got.PeakWindows != "" || !strings.Contains(got.Reason, "28 days") ||
		len(got.Hours) != 0 {
		t.Fatalf("27 days 23 hours: %+v %v", got, err)
	}
}

func TestSuggestPeakWindowsNeedsOKCoverageOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	breaks := func(at time.Time) bool { return at.Hour()%3 == 0 } // a third of the hours
	seedRollup(t, ctx, pool, peakFrom, mondayMorningPeaks(), breaks)
	got, err := SuggestPeakWindows(ctx, pool, peakNow)
	if err != nil || got.PeakWindows != "" || got.CoverageStatus != "mostly_breaks" ||
		!strings.Contains(got.Reason, "mostly_breaks") {
		t.Fatalf("mostly_breaks coverage: %+v %v", got, err)
	}
}

func TestSuggestPeakWindowsEmptyRollupOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	seedRollup(t, ctx, pool, peakNow, nil, nil) // no rows at all
	got, err := SuggestPeakWindows(ctx, pool, peakNow)
	if err != nil || got.PeakWindows != "" || got.Reason == "" || got.Hours == nil ||
		len(got.Hours) != 0 {
		t.Fatalf("empty rollup: %+v %v", got, err)
	}
}

func TestSuggestPeakWindowsReadErrorOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	if _, err := SuggestPeakWindows(ctx, other, peakNow); err == nil ||
		!strings.Contains(err.Error(), "peak") {
		t.Fatalf("closed pool: err = %v, want a peak-window read error", err)
	}
}

// The coverage rows outlive the hourly rows (query_rollup_daily_days is
// longer than query_rollup_hourly_days, which is at most snapshots_days):
// with 28 days of coverage but only the last 21 days of hourly rollup, the
// purged first week would read as idle and 3 weeks would pass for "3 of
// 4". It suggests nothing and names the retention.
func TestSuggestPeakWindowsNeedsTwentyEightDaysOfHourlyRowsOnLiveDB(t *testing.T) {
	pool, ctx := livePool(t)
	kept := map[time.Time]float64{}
	cut := peakFrom.Add(7 * 24 * time.Hour)
	for at, ms := range peakLoad(peakAt{time.Monday, 9, []int{1, 2, 3}},
		peakAt{time.Monday, 10, []int{1, 2, 3}}) {
		if !at.Before(cut) {
			kept[at] = ms
		}
	}
	seedRollup(t, ctx, pool, peakFrom.Add(-24*time.Hour), kept, nil)
	got, err := SuggestPeakWindows(ctx, pool, peakNow)
	if err != nil || got.PeakWindows != "" || got.CoverageStatus != "ok" ||
		!strings.Contains(got.Reason, "28 days of hourly rollup") ||
		!strings.Contains(got.Reason, cut.Format(time.RFC3339)) ||
		!strings.Contains(got.Reason, "query_rollup_hourly_days") || len(got.Hours) != 0 {
		t.Fatalf("21 days of hourly rows under 29 days of coverage: %+v %v", got, err)
	}
}
