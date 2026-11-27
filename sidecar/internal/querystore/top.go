package querystore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// The workload explorer's window readers (market-01): the heaviest
// queries of a window and one query's history. Both keep the anchor and
// epoch rules of intervals.go: a window starts at each query's last
// sample before it (within AnchorLookback), an absent sample means
// unchanged, and a pair of samples across an epoch break is left out, so
// a reset mid-window splits the window instead of producing a negative
// or a doubled delta.

// Orders of a top-queries window.
const (
	OrderLoad  = "load"  // execution time per wall-clock second
	OrderCalls = "calls" // calls in the window
	OrderMean  = "mean"  // mean execution time in the window
	OrderRows  = "rows"  // rows in the window
)

// Coverage says what a window's samples can measure.
const (
	CoverageNone          = "none"           // no sample in the window
	CoverageNotMeasurable = "not_measurable" // samples, but no two in one epoch
	CoveragePartial       = "partial"        // an epoch break or an unmeasurable query
	CoverageFull          = "full"
)

// MaxTopScan bounds the queries one top read returns.
const MaxTopScan = 500

// ErrInvalidRequest marks a window or request a reader refuses.
var ErrInvalidRequest = errors.New("querystore: invalid request")

var topOrders = map[string]bool{OrderLoad: true, OrderCalls: true, OrderMean: true,
	OrderRows: true}

// TopRequest asks for the heaviest queries of [From, To].
type TopRequest struct {
	From, To time.Time
	Order    string
	Limit    int
}

// WindowStat is one query's work in a window: the sums of the deltas of
// its valid sample pairs.
type WindowStat struct {
	QueryID     int64
	Samples     int // samples inside the window
	Intervals   int // sample pairs measured (same epoch)
	EpochBreaks int // sample pairs left out across an epoch break
	Calls       int64
	TotalMs     float64
	Rows        int64
}

// MeanMs is the window's mean execution time (0 without calls).
func (w WindowStat) MeanMs() float64 {
	if w.Calls <= 0 {
		return 0
	}
	return w.TotalMs / float64(w.Calls)
}

// Load is the query's execution seconds per wall-clock second of window:
// on average, how many sessions ran it.
func (w WindowStat) Load(window time.Duration) float64 {
	if window <= 0 {
		return 0
	}
	return w.TotalMs / 1000 / window.Seconds()
}

// TopResult is a window's ranked queries. Queries is never nil.
type TopResult struct {
	Coverage      string
	Queries       []WindowStat
	NotMeasurable int // queries sampled without a measurable pair
	EpochBreaks   int // pairs left out across epoch breaks, all queries
	Window        time.Duration
}

// ValidateTop says why req cannot be read (ErrInvalidRequest), or nil.
func ValidateTop(req TopRequest) error {
	switch {
	case req.From.IsZero() || req.To.IsZero():
		return fmt.Errorf("%w: the window needs a start and an end", ErrInvalidRequest)
	case !req.To.After(req.From):
		return fmt.Errorf("%w: the window ends (%v) at or before it starts (%v)",
			ErrInvalidRequest, req.To, req.From)
	case req.Limit < 1 || req.Limit > MaxTopScan:
		return fmt.Errorf("%w: limit %d is outside 1..%d", ErrInvalidRequest, req.Limit,
			MaxTopScan)
	case !topOrders[req.Order]:
		return fmt.Errorf("%w: order %q is not load, calls, mean or rows", ErrInvalidRequest,
			req.Order)
	}
	return nil
}

// windowSamplesSQL is every sample of the window [$1, $2] and of the
// AnchorLookback ($3) before it, each with the sample before it of the
// same query (alias s, prev_ columns). Readers keep the rows inside the
// window: their previous sample is the anchor or an in-window sample.
var windowSamplesSQL = `SELECT q.queryid, q.captured_at, q.calls, q.total_exec_time,
		q.rows, q.stats_epoch, q.plan_hash,
		lag(q.calls) OVER w AS prev_calls,
		lag(q.total_exec_time) OVER w AS prev_total_exec_time,
		lag(q.rows) OVER w AS prev_rows,
		lag(q.stats_epoch) OVER w AS prev_stats_epoch
	FROM sage.query_store q
	WHERE {db:q} AND q.captured_at >= $1::timestamptz - $3::interval
		AND q.captured_at <= $2 %s
	WINDOW w AS (PARTITION BY q.queryid ORDER BY q.captured_at, q.id)`

// topWindowSQL sums each query's valid pairs in [$1, $2].
var topWindowSQL = `/* pg_sage */ WITH s AS (` + fmt.Sprintf(windowSamplesSQL, "") + `
), p AS (
	SELECT s.*, s.prev_calls IS NOT NULL AND NOT ` + epochBreak("s.", "s.prev_") + ` AS ok,
		s.prev_calls IS NOT NULL AND ` + epochBreak("s.", "s.prev_") + ` AS brk
	FROM s WHERE s.captured_at >= $1
)
SELECT p.queryid, count(*)::int, (count(*) FILTER (WHERE p.ok))::int,
	(count(*) FILTER (WHERE p.brk))::int,
	COALESCE(sum(p.calls - p.prev_calls) FILTER (WHERE p.ok), 0)::bigint,
	COALESCE(sum(p.total_exec_time - p.prev_total_exec_time) FILTER (WHERE p.ok), 0)::float8,
	COALESCE(sum(p.rows - p.prev_rows) FILTER (WHERE p.ok), 0)::bigint
FROM p GROUP BY p.queryid`

// Top returns the heaviest queries of the window by req.Order, read in
// db's history store (histstore.Resolve) with one statement.
func Top(ctx context.Context, db any, req TopRequest) (TopResult, error) {
	if err := ValidateTop(req); err != nil {
		return TopResult{}, err
	}
	rows, err := histstore.Resolve(db).Query(ctx, topWindowSQL, req.From, req.To,
		AnchorLookback)
	if err != nil {
		return TopResult{}, fmt.Errorf("top queries %v..%v: %w", req.From, req.To, err)
	}
	stats, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WindowStat, error) {
		var w WindowStat
		err := r.Scan(&w.QueryID, &w.Samples, &w.Intervals, &w.EpochBreaks, &w.Calls,
			&w.TotalMs, &w.Rows)
		return w, err
	})
	if err != nil {
		return TopResult{}, fmt.Errorf("top queries %v..%v: %w", req.From, req.To, err)
	}
	return rankTop(stats, req), nil
}

// rankTop ranks the measurable queries by req.Order (ties by queryid),
// keeps req.Limit, and says what the window covers.
func rankTop(stats []WindowStat, req TopRequest) TopResult {
	res := TopResult{Queries: []WindowStat{}, Window: req.To.Sub(req.From)}
	for _, s := range stats {
		res.EpochBreaks += s.EpochBreaks
		if s.Intervals == 0 {
			res.NotMeasurable++
			continue
		}
		res.Queries = append(res.Queries, s)
	}
	key := orderKey(req.Order)
	sort.SliceStable(res.Queries, func(i, j int) bool {
		a, b := key(res.Queries[i]), key(res.Queries[j])
		if a != b {
			return a > b
		}
		return res.Queries[i].QueryID < res.Queries[j].QueryID
	})
	if len(res.Queries) > req.Limit {
		res.Queries = res.Queries[:req.Limit]
	}
	res.Coverage = coverageOf(len(stats), len(stats)-res.NotMeasurable,
		res.NotMeasurable > 0 || res.EpochBreaks > 0)
	return res
}

func orderKey(order string) func(WindowStat) float64 {
	switch order {
	case OrderCalls:
		return func(w WindowStat) float64 { return float64(w.Calls) }
	case OrderMean:
		return WindowStat.MeanMs
	case OrderRows:
		return func(w WindowStat) float64 { return float64(w.Rows) }
	}
	return func(w WindowStat) float64 { return w.TotalMs }
}

// coverageOf names a window's coverage from how many things it sampled,
// how many of them it measured, and whether any measurement has a gap.
func coverageOf(sampled, measured int, gap bool) string {
	switch {
	case sampled == 0:
		return CoverageNone
	case measured == 0:
		return CoverageNotMeasurable
	case gap:
		return CoveragePartial
	}
	return CoverageFull
}
