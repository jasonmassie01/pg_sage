package querystore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// maxHistorySamples bounds one history read: a week of one-minute cycles
// plus the anchor lookback.
const maxHistorySamples = 12_000

// HistoryBucket is one time bucket of a query's window: the sums of the
// deltas of its valid sample pairs, and whether a pair in it was left
// out across an epoch break.
type HistoryBucket struct {
	Start      time.Time
	Calls      int64
	TotalMs    float64
	MeanMs     float64
	Rows       int64
	EpochBreak bool
}

// PlanMarker is a change of the query's plan fingerprint (plan_hash): the
// first sample stamped with To after samples stamped with From.
type PlanMarker struct {
	At       time.Time
	From, To string
}

// History is one query's window. Buckets and Markers are never nil.
type History struct {
	Coverage string
	Samples  int // samples inside the window
	Buckets  []HistoryBucket
	Markers  []PlanMarker
}

// samplePoint is one sample of a history read with its delta to the
// sample before it.
type samplePoint struct {
	at       time.Time
	planHash *string
	inWindow bool
	hasPrev  bool
	brk      bool
	dCalls   int64
	dTotal   float64
	dRows    int64
}

// queryHistorySQL is query $4's newest $5 samples of [$1, $2] and of the
// lookback $3 before it, oldest first, each with its delta to the sample
// before.
var queryHistorySQL = `/* pg_sage */ WITH s AS (` +
	fmt.Sprintf(windowSamplesSQL, "AND q.queryid = $4") + `
), newest AS (
	SELECT s.captured_at, s.plan_hash, s.captured_at >= $1 AS in_window,
		s.prev_calls IS NOT NULL AS has_prev,
		COALESCE(` + epochBreak("s.", "s.prev_") + `, false) AS brk,
		COALESCE(s.calls - s.prev_calls, 0)::bigint AS d_calls,
		COALESCE(s.total_exec_time - s.prev_total_exec_time, 0)::float8 AS d_total,
		COALESCE(s.rows - s.prev_rows, 0)::bigint AS d_rows
	FROM s ORDER BY s.captured_at DESC LIMIT $5
)
SELECT captured_at, plan_hash, in_window, has_prev, brk, d_calls, d_total, d_rows
FROM newest ORDER BY captured_at`

// QueryHistory returns queryid's window [from, to] in buckets of bucket,
// with its plan fingerprint changes, read in db's history store.
func QueryHistory(ctx context.Context, db any, queryid int64, from, to time.Time,
	bucket time.Duration) (History, error) {
	if !to.After(from) || from.IsZero() {
		return History{}, fmt.Errorf("%w: query %d window %v..%v is empty",
			ErrInvalidRequest, queryid, from, to)
	}
	if bucket <= 0 {
		return History{}, fmt.Errorf("%w: query %d bucket must be positive, got %v",
			ErrInvalidRequest, queryid, bucket)
	}
	rows, err := histstore.Resolve(db).Query(ctx, queryHistorySQL, from, to, AnchorLookback,
		queryid, maxHistorySamples)
	if err != nil {
		return History{}, fmt.Errorf("query %d history: %w", queryid, err)
	}
	points, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (samplePoint, error) {
		var p samplePoint
		err := r.Scan(&p.at, &p.planHash, &p.inWindow, &p.hasPrev, &p.brk, &p.dCalls,
			&p.dTotal, &p.dRows)
		return p, err
	})
	if err != nil {
		return History{}, fmt.Errorf("query %d history: %w", queryid, err)
	}
	return buildHistory(points, bucket), nil
}

// buildHistory sums the in-window deltas per bucket (time.Truncate: a
// bucket that divides a day starts on the wall clock) and finds the plan
// changes. A pair across an epoch break only
// flags its bucket.
func buildHistory(points []samplePoint, bucket time.Duration) History {
	h := History{Buckets: []HistoryBucket{}, Markers: planMarkers(points)}
	measured, breaks := 0, false
	for _, p := range points {
		if !p.inWindow {
			continue
		}
		h.Samples++
		if !p.hasPrev {
			continue
		}
		start := p.at.UTC().Truncate(bucket)
		if n := len(h.Buckets); n == 0 || !h.Buckets[n-1].Start.Equal(start) {
			h.Buckets = append(h.Buckets, HistoryBucket{Start: start})
		}
		b := &h.Buckets[len(h.Buckets)-1]
		if p.brk {
			b.EpochBreak, breaks = true, true
			continue
		}
		measured++
		b.Calls += p.dCalls
		b.TotalMs += p.dTotal
		b.Rows += p.dRows
	}
	for i := range h.Buckets {
		if h.Buckets[i].Calls > 0 {
			h.Buckets[i].MeanMs = h.Buckets[i].TotalMs / float64(h.Buckets[i].Calls)
		}
	}
	h.Coverage = coverageOf(h.Samples, measured, breaks)
	return h
}

// planMarkers lists the in-window samples whose fingerprint differs from
// the last fingerprint before them (an unfingerprinted sample keeps the
// last one). The first fingerprint is not a change.
func planMarkers(points []samplePoint) []PlanMarker {
	out := []PlanMarker{}
	last := ""
	for _, p := range points {
		if p.planHash == nil || *p.planHash == "" {
			continue
		}
		if p.inWindow && last != "" && *p.planHash != last {
			out = append(out, PlanMarker{At: p.at, From: last, To: *p.planHash})
		}
		last = *p.planHash
	}
	return out
}
