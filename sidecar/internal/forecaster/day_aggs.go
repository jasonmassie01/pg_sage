package forecaster

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// queryAggs sums per-day call deltas over the lookback's daily samples.
// pg_stat_statements counters are cumulative since the last reset, so each
// sample contributes calls - previous calls for the same queryid; a drop
// (reset or eviction) contributes the new count, and a queryid's first
// sample in the window contributes 0 because its baseline is unknown
// (C10). Only each day's first and last snapshot are sampled: for
// monotonic counters the daily totals telescope to the same sums. Each day
// carries its snapshot count and span (forecast-08); a lookback of 0 or
// less reads nothing.
func (h *dayHistory) queryAggs(ctx context.Context, pool *pgxpool.Pool,
	days int) ([]DayQueryAgg, error) {
	if days <= 0 {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	first, last, counts, err := dayPicks(ctx, pool, "queries", days)
	if err != nil {
		return nil, err
	}
	samples := append(first, last...)
	cached := keepOnly(samples, h.queries)
	err = decodeBatches(missingIDs(samples, cached), func(ids []int64) error {
		return decodeQueryCalls(ctx, pool, ids, cached)
	})
	if err != nil {
		return nil, err
	}
	h.queries = cached
	aggs := sumCallDeltas(dedupeSamples(samples), cached)
	for i := range aggs {
		c := counts[aggs[i].Day.Unix()]
		aggs[i].Samples, aggs[i].FirstAt, aggs[i].LastAt = c.samples, c.firstAt, c.lastAt
	}
	return aggs, nil
}

// dedupeSamples keeps one sample per snapshot (a day's only snapshot is
// both its first and last), in time order.
func dedupeSamples(samples []daySample) []daySample {
	seen := map[int64]bool{}
	out := make([]daySample, 0, len(samples))
	for _, s := range samples {
		if !seen[s.id] {
			seen[s.id] = true
			out = append(out, s)
		}
	}
	sortSamples(out)
	return out
}

// sumCallDeltas is the SQL aggregation the forecaster used: per queryid a
// lag over the samples, per day the sum of the deltas (a day appears when
// one of its samples has an element; NULL deltas are not summed). The
// deltas counted at a day's first sample since a sample of an earlier day
// are also kept as the day's carry (forecast-08).
func sumCallDeltas(samples []daySample, decoded map[int64][]queryCalls) []DayQueryAgg {
	prev := map[callKey]*int64{}
	var out []DayQueryAgg
	var before *daySample
	for i, s := range samples {
		elems := decoded[s.id]
		if len(elems) == 0 {
			continue
		}
		if len(out) == 0 || !out[len(out)-1].Day.Equal(s.day) {
			out = append(out, DayQueryAgg{Day: s.day})
		}
		day := &out[len(out)-1]
		carry := before != nil && !before.day.Equal(s.day)
		if carry {
			day.CarryFrom, day.CarryTo, day.CarryFromDay = before.at, s.at, before.day
		}
		for _, e := range elems {
			d := callDelta(prev, e)
			day.TotalCalls += d
			if carry {
				day.CarryCalls += d
			}
		}
		before = &samples[i]
	}
	return out
}

// callKey identifies a statement across samples (a NULL queryid is one).
type callKey struct {
	null bool
	qid  int64
}

// callDelta is e's calls since the statement's previous sample: 0 without
// one, the new count after a reset or eviction. It records e in prev.
func callDelta(prev map[callKey]*int64, e queryCalls) float64 {
	k := callKey{null: e.qid == nil}
	if e.qid != nil {
		k.qid = *e.qid
	}
	p, seen := prev[k]
	prev[k] = e.calls
	switch {
	case !seen || p == nil || e.calls == nil:
		return 0
	case *e.calls >= *p:
		return float64(*e.calls - *p)
	default:
		return float64(*e.calls)
	}
}

func decodeQueryCalls(ctx context.Context, pool *pgxpool.Pool, ids []int64,
	into map[int64][]queryCalls) error {
	for _, id := range ids {
		into[id] = nil
	}
	rows, err := histstore.Resolve(pool).Query(ctx, queryCallsSQL, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var e queryCalls
		if err := rows.Scan(&id, &e.qid, &e.calls); err != nil {
			return fmt.Errorf("scan query sample: %w", err)
		}
		into[id] = append(into[id], e)
	}
	return rows.Err()
}
