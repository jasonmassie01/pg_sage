package querystore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// No concurrent access tests: both readers are single read-only
// statements with no shared state; the registry they resolve through has
// its own concurrency test (histstore).

// goldenVerifySQL is verify's queryIntervalsSQL as it was before the
// readers moved here (collection-12), kept verbatim as the parity golden.
const goldenVerifySQL = `/* pg_sage */ WITH anchor AS (
		SELECT max(a.captured_at) AS at FROM sage.query_store a
		WHERE {db:a} AND a.queryid=$1 AND a.captured_at < $2
			AND a.captured_at >= $2 - $4::interval
	), samples AS (
		SELECT q.captured_at,
			q.calls - lag(q.calls) OVER w AS d_calls,
			q.total_exec_time - lag(q.total_exec_time) OVER w AS d_time,
			q.calls < lag(q.calls) OVER w
				OR q.total_exec_time < lag(q.total_exec_time) OVER w
				OR (row_number() OVER w > 1
					AND q.stats_epoch IS DISTINCT FROM lag(q.stats_epoch) OVER w)
				AS epoch_break
		FROM sage.query_store q
		WHERE {db:q} AND q.queryid=$1
			AND q.captured_at BETWEEN COALESCE((SELECT at FROM anchor), $2) AND $3
		WINDOW w AS (ORDER BY q.captured_at, q.id)
	)
	SELECT COALESCE(sum(d_calls), 0)::bigint, COALESCE(sum(d_time), 0)::float8,
		COALESCE(bool_or(epoch_break), false)
	FROM samples WHERE d_calls IS NOT NULL
	GROUP BY floor(extract(epoch FROM captured_at) / $5)
	ORDER BY floor(extract(epoch FROM captured_at) / $5)`

// goldenSLODeltasSQL is the SLO latency proxy's deltasSQL as it was before
// the readers moved here (collection-12), kept verbatim as the golden.
const goldenSLODeltasSQL = `/* pg_sage sre:slo_proxy */
SELECT (c.calls - p.calls)::float8, (c.total_exec_time - p.total_exec_time)::float8,
       bool_or(c.stats_epoch IS DISTINCT FROM p.stats_epoch OR c.calls < p.calls) OVER ()
FROM sage.query_store c
CROSS JOIN LATERAL (
    SELECT q.calls, q.total_exec_time, q.stats_epoch FROM sage.query_store q
    WHERE {db:q} AND q.queryid = c.queryid AND q.captured_at <= $2
      AND q.captured_at >= $2 - $4::interval
    ORDER BY q.captured_at DESC, q.id DESC LIMIT 1) p
WHERE {db:c} AND c.captured_at = $1
ORDER BY c.total_exec_time - p.total_exec_time DESC
LIMIT $3`

// seedRow is one query_store sample at an offset from a case's origin.
type seedRow struct {
	qid   int64
	at    time.Duration
	calls int64
	total float64
	epoch *time.Time
}

func seedRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, origin time.Time,
	rows []seedRow) {
	t.Helper()
	for _, r := range rows {
		insertAt(t, ctx, pool, r.qid, origin.Add(r.at), r.calls, r.total, r.epoch)
	}
}

func goldenIntervals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	from, to time.Time, bucket time.Duration) []Interval {
	t.Helper()
	rows, err := histstore.NewMonitored(pool).Query(ctx, goldenVerifySQL, qid, from, to,
		AnchorLookback, bucket.Seconds())
	if err != nil {
		t.Fatalf("golden verify SQL: %v", err)
	}
	defer rows.Close()
	var out []Interval
	for rows.Next() {
		var iv Interval
		if err := rows.Scan(&iv.Calls, &iv.TotalMs, &iv.EpochBreak); err != nil {
			t.Fatal(err)
		}
		out = append(out, iv)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameIntervals(a, b []Interval) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intervalTotals(ivs []Interval) (calls int64, broken bool) {
	for _, iv := range ivs {
		calls += iv.Calls
		broken = broken || iv.EpochBreak
	}
	return calls, broken
}

// intervalCase is one seeded window of the verify parity test.
type intervalCase struct {
	name       string
	rows       []seedRow
	wantN      int
	wantCalls  int64
	wantBroken bool
}

// verifyGoldenCases are the verify parity windows of query qid, with
// statistics epochs a and b.
func verifyGoldenCases(qid int64, a, b time.Time) []intervalCase {
	return []intervalCase{
		{"steady with anchor", []seedRow{{qid, -50 * time.Minute, 100, 1000, &a},
			{qid, -20 * time.Minute, 150, 2000, &a}, {qid, -5 * time.Minute, 200, 2600, &a}},
			2, 100, false},
		{"reset mid-window", []seedRow{{qid, -25 * time.Minute, 100, 1000, &a},
			{qid, -15 * time.Minute, 5, 50, &a}, {qid, -5 * time.Minute, 300, 9000, &a}},
			2, 200, true},
		{"idle anchor inside lookback", []seedRow{
			{qid, -30*time.Minute - AnchorLookback + time.Minute, 100, 1000, &a},
			{qid, -10 * time.Minute, 200, 4000, &a}}, 1, 100, false},
		{"idle anchor outside lookback", []seedRow{
			{qid, -30*time.Minute - AnchorLookback - time.Minute, 100, 1000, &a},
			{qid, -10 * time.Minute, 200, 4000, &a}}, 0, 0, false},
		{"unknown epoch next to known", []seedRow{{qid, -25 * time.Minute, 100, 1000, nil},
			{qid, -10 * time.Minute, 200, 4000, &a}}, 1, 100, true},
		{"counter decrease", []seedRow{{qid, -25 * time.Minute, 100, 1000, &a},
			{qid, -10 * time.Minute, 200, 500, &a}}, 1, 100, true},
		{"epoch change after regrowth", []seedRow{{qid, -25 * time.Minute, 100, 1000, &a},
			{qid, -10 * time.Minute, 400, 9000, &b}}, 1, 300, true},
		{"sample exactly at window start", []seedRow{{qid, -40 * time.Minute, 50, 500, &a},
			{qid, -30 * time.Minute, 100, 1000, &a}, {qid, -10 * time.Minute, 200, 3000, &a}},
			2, 150, false},
	}
}

// Parity: QueryIntervals answers exactly what verify's SQL answered.
func TestQueryIntervalsMatchesVerifyGolden(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	a := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	const qid = int64(7_400_000_001)
	for i, tc := range verifyGoldenCases(qid, a, a.Add(24*time.Hour)) {
		t.Run(tc.name, func(t *testing.T) {
			clearQueryIDs(t, ctx, pool, qid, qid)
			to := time.Date(2025, 3, 1+i, 12, 0, 0, 0, time.UTC)
			from := to.Add(-30 * time.Minute)
			seedRows(t, ctx, pool, to, tc.rows)
			want := goldenIntervals(t, ctx, pool, qid, from, to, 5*time.Minute)
			got, err := QueryIntervals(ctx, pool, qid, from, to, 5*time.Minute)
			if err != nil {
				t.Fatalf("QueryIntervals: %v", err)
			}
			if !sameIntervals(got, want) {
				t.Fatalf("QueryIntervals = %+v, golden = %+v", got, want)
			}
			calls, broken := intervalTotals(got)
			if len(got) != tc.wantN || calls != tc.wantCalls || broken != tc.wantBroken {
				t.Fatalf("intervals = %+v; want %d buckets, %d calls, broken=%v",
					got, tc.wantN, tc.wantCalls, tc.wantBroken)
			}
		})
	}
}

func TestQueryIntervalsRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, bucket := range []time.Duration{0, -time.Second} {
		_, err := QueryIntervals(ctx, histstore.NewMonitored(nil), 1, now.Add(-time.Hour), now,
			bucket)
		if err == nil || !strings.Contains(err.Error(), "bucket") {
			t.Fatalf("bucket %v: err = %v, want a bucket error", bucket, err)
		}
	}
	_, err := QueryIntervals(ctx, nil, 1, now.Add(-time.Hour), now, time.Minute)
	if !errors.Is(err, histstore.ErrNoDatabase) {
		t.Fatalf("nil database: err = %v, want ErrNoDatabase", err)
	}
}

// An empty window or an unknown queryid has no intervals, not an error.
func TestQueryIntervalsEmptyWindow(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	got, err := QueryIntervals(ctx, pool, -99_887_766, time.Now().Add(-time.Hour),
		time.Now(), time.Minute)
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown queryid = %+v, %v; want no intervals", got, err)
	}
}

type deltaRow struct {
	calls, total float64
	reset        bool
}

func goldenDeltas(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cur, prev time.Time,
	limit int) []deltaRow {
	t.Helper()
	rows, err := histstore.NewMonitored(pool).Query(ctx, goldenSLODeltasSQL, cur, prev, limit,
		AnchorLookback)
	if err != nil {
		t.Fatalf("golden SLO SQL: %v", err)
	}
	defer rows.Close()
	var out []deltaRow
	for rows.Next() {
		var d deltaRow
		if err := rows.Scan(&d.calls, &d.total, &d.reset); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameDeltas compares CaptureDeltas with the golden rows: the same deltas
// in the same order, and the golden's per-row reset flag as the result's.
func sameDeltas(got []Delta, reset bool, want []deltaRow) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Calls != want[i].calls || got[i].TotalMs != want[i].total ||
			want[i].reset != reset {
			return false
		}
	}
	return true
}

// sloCase is one seeded capture pair of the SLO parity test.
type sloCase struct {
	name      string
	rows      []seedRow
	limit     int
	wantN     int
	wantReset bool
}

// The SLO parity test's queries and where its previous capture sits.
const (
	sloQ1, sloQ2, sloQ3, sloQ4 = int64(7_400_000_011), int64(7_400_000_012),
		int64(7_400_000_013), int64(7_400_000_014)
	sloPrevAt = -5 * time.Minute
)

// sloGoldenCases are the SLO parity capture pairs, with statistics epochs
// a and b.
func sloGoldenCases(a, b time.Time) []sloCase {
	q1, q2, q3, q4, prevAt := sloQ1, sloQ2, sloQ3, sloQ4, sloPrevAt
	return []sloCase{
		{"anchors inside and outside lookback", []seedRow{
			{q1, prevAt, 100, 1000, &a}, {q1, 0, 150, 2000, &a},
			{q2, prevAt - AnchorLookback + time.Minute, 10, 100, &a}, {q2, 0, 30, 1500, &a},
			{q3, prevAt - AnchorLookback - time.Minute, 10, 100, &a}, {q3, 0, 20, 9000, &a},
			{q4, 0, 5, 50, &a}}, 10, 2, false},
		{"top-N limit", []seedRow{
			{q1, prevAt, 100, 1000, &a}, {q1, 0, 150, 2000, &a},
			{q2, prevAt, 10, 100, &a}, {q2, 0, 30, 1500, &a}}, 1, 1, false},
		{"reset in one query", []seedRow{
			{q1, prevAt, 100, 1000, &a}, {q1, 0, 150, 2000, &a},
			{q2, prevAt, 100, 1000, &a}, {q2, 0, 5, 50, &a}}, 10, 2, true},
		{"unknown epoch next to known", []seedRow{
			{q1, prevAt, 100, 1000, nil}, {q1, 0, 150, 2000, &a}}, 10, 1, true},
		{"epoch change after regrowth", []seedRow{
			{q1, prevAt, 100, 1000, &a}, {q1, 0, 400, 9000, &b}}, 10, 1, true},
	}
}

// Parity: CaptureDeltas answers what the SLO latency proxy's SQL answered.
func TestCaptureDeltasMatchesSLOGolden(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	a := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	for i, tc := range sloGoldenCases(a, a.Add(24*time.Hour)) {
		t.Run(tc.name, func(t *testing.T) {
			clearQueryIDs(t, ctx, pool, sloQ1, sloQ4)
			cur := time.Date(2025, 4, 1+i, 12, 0, 0, 0, time.UTC)
			prev := cur.Add(sloPrevAt)
			seedRows(t, ctx, pool, cur, tc.rows)
			want := goldenDeltas(t, ctx, pool, cur, prev, tc.limit)
			got, reset, err := CaptureDeltas(ctx, pool, cur, prev, tc.limit)
			if err != nil {
				t.Fatalf("CaptureDeltas: %v", err)
			}
			if !sameDeltas(got, reset, want) {
				t.Fatalf("CaptureDeltas = %+v reset=%v, golden = %+v", got, reset, want)
			}
			if len(got) != tc.wantN || reset != tc.wantReset {
				t.Fatalf("deltas = %+v reset=%v; want %d rows, reset=%v", got, reset,
					tc.wantN, tc.wantReset)
			}
		})
	}
}

// One epoch rule for both readers: a total that fell while calls grew is a
// reset for the SLO proxy too (the old proxy SQL only dropped that row).
func TestCaptureDeltasTotalDecreaseIsAReset(t *testing.T) {
	pool, ctx := requireDB(t)
	defer pool.Close()
	a := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	const q1, q2 = int64(7_400_000_021), int64(7_400_000_022)
	clearQueryIDs(t, ctx, pool, q1, q2)
	cur := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	seedRows(t, ctx, pool, cur, []seedRow{
		{q1, -5 * time.Minute, 100, 1000, &a}, {q1, 0, 150, 2000, &a},
		{q2, -5 * time.Minute, 100, 1000, &a}, {q2, 0, 150, 400, &a}})
	got, reset, err := CaptureDeltas(ctx, pool, cur, cur.Add(-5*time.Minute), 10)
	if err != nil || !reset || len(got) != 2 {
		t.Fatalf("CaptureDeltas = %+v reset=%v err=%v; want 2 rows and a reset", got, reset,
			err)
	}
	if got[0].Calls != 50 || got[0].TotalMs != 1000 {
		t.Fatalf("top delta = %+v, want 50 calls / 1000 ms", got[0])
	}
}

func TestCaptureDeltasRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, limit := range []int{0, -1} {
		_, _, err := CaptureDeltas(ctx, histstore.NewMonitored(nil), now, now.Add(-time.Minute),
			limit)
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("limit %d: err = %v, want a limit error", limit, err)
		}
	}
	_, _, err := CaptureDeltas(ctx, nil, now, now.Add(-time.Minute), 5)
	if !errors.Is(err, histstore.ErrNoDatabase) {
		t.Fatalf("nil database: err = %v, want ErrNoDatabase", err)
	}
	if _, err := RecentCaptures(ctx, nil, time.Minute, 2); !errors.Is(err,
		histstore.ErrNoDatabase) {
		t.Fatalf("RecentCaptures nil database: err = %v, want ErrNoDatabase", err)
	}
	if _, err := RecentCaptures(ctx, nil, time.Minute, 0); err == nil {
		t.Fatal("RecentCaptures with n=0 must be refused")
	}
}

func TestRecentCapturesNewestFirst(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	ctx := context.Background()
	const qid = int64(7_400_000_031)
	var now time.Time
	if err := p.Monitored.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	for _, ago := range []time.Duration{40, 20, 10, 10, 5} {
		insertScoped(t, ctx, p.Monitored, qid, now.Add(-ago*time.Minute), 1, 1, 0)
	}
	got, err := RecentCaptures(ctx, p.Monitored, 30*time.Minute, 2)
	if err != nil {
		t.Fatalf("RecentCaptures: %v", err)
	}
	if len(got) != 2 || !got[0].Equal(now.Add(-5*time.Minute)) ||
		!got[1].Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("captures = %v, want the 5 and 10 minute old ones, newest first", got)
	}
	got, err = RecentCaptures(ctx, p.Monitored, 30*time.Minute, 5)
	if err != nil || len(got) != 3 {
		t.Fatalf("captures within 30 minutes = %v, %v; want 3 distinct", got, err)
	}
}

// Meta mode: the readers scope to their database's rows; another
// database's rows at the same times and query ids change nothing.
func TestIntervalReadersAgreeInBothHistoryModes(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	const qid = int64(7_400_000_041)
	cur := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	seed := []seedRow{{qid, -20 * time.Minute, 100, 1000, nil},
		{qid, -10 * time.Minute, 150, 2000, nil}, {qid, 0, 180, 2600, nil}}
	type answer struct {
		ivs    []Interval
		deltas []Delta
		reset  bool
	}
	got := map[histstore.Mode]answer{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		target, id := p.Monitored, 0
		if mode == histstore.ModeMeta {
			target, id = p.Meta, histfixture.DatabaseID
			p.Noise(t, cur, qid)
			p.Noise(t, cur.Add(-10*time.Minute), qid)
		}
		for _, r := range seed {
			insertScoped(t, ctx, target, r.qid, cur.Add(r.at), r.calls, r.total, id)
		}
		var ans answer
		var err error
		if ans.ivs, err = QueryIntervals(ctx, p.Monitored, qid, cur.Add(-15*time.Minute), cur,
			time.Minute); err != nil {
			t.Fatalf("%s QueryIntervals: %v", mode, err)
		}
		if ans.deltas, ans.reset, err = CaptureDeltas(ctx, p.Monitored, cur,
			cur.Add(-10*time.Minute), 5); err != nil {
			t.Fatalf("%s CaptureDeltas: %v", mode, err)
		}
		got[mode] = ans
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if calls, _ := intervalTotals(mon.ivs); calls != 80 || len(mon.ivs) != 2 {
		t.Fatalf("monitored intervals = %+v, want 2 buckets of 80 calls", mon.ivs)
	}
	if !sameIntervals(mon.ivs, meta.ivs) {
		t.Fatalf("intervals differ: monitored %+v, meta %+v", mon.ivs, meta.ivs)
	}
	if len(meta.deltas) != 1 || meta.deltas[0] != (Delta{Calls: 30, TotalMs: 600}) ||
		meta.reset || len(mon.deltas) != 1 || mon.deltas[0] != meta.deltas[0] {
		t.Fatalf("deltas differ or wrong: monitored %+v, meta %+v reset=%v", mon.deltas,
			meta.deltas, meta.reset)
	}
}

// insertScoped writes one sample into pool, as databaseID's row when it is
// positive (the meta database's store).
func insertScoped(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	at time.Time, calls int64, total float64, databaseID int) {
	t.Helper()
	sql := `INSERT INTO sage.query_store
		(queryid, captured_at, calls, total_exec_time, mean_exec_time, rows)
		VALUES ($1, $2, $3, $4, 0, 0)`
	args := []any{qid, at, calls, total}
	if databaseID > 0 {
		sql = `INSERT INTO sage.query_store (queryid, captured_at, calls, total_exec_time,
			mean_exec_time, rows, database_id) VALUES ($1, $2, $3, $4, 0, 0, $5)`
		args = append(args, databaseID)
	}
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}
