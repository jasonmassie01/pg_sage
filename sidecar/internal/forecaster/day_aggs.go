package forecaster

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// queryAggs sums per-day call deltas over the lookback's daily samples.
// pg_stat_statements counters are cumulative since the last reset, so each
// sample contributes calls - previous calls for the same queryid; a drop
// (reset or eviction) contributes the new count, and a queryid's first
// sample in the window contributes 0 because its baseline is unknown
// (C10). Only each day's first and last snapshot are sampled: for
// monotonic counters the daily totals telescope to the same sums.
func (h *dayHistory) queryAggs(ctx context.Context, pool *pgxpool.Pool,
	days int) ([]DayQueryAgg, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	first, last, err := dayPicks(ctx, pool, "queries", days)
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
	return sumCallDeltas(dedupeSamples(samples), cached), nil
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
// one of its samples has an element; NULL deltas are not summed).
func sumCallDeltas(samples []daySample, decoded map[int64][]queryCalls) []DayQueryAgg {
	type key struct {
		null bool
		qid  int64
	}
	prev := map[key]*int64{}
	var out []DayQueryAgg
	for _, s := range samples {
		elems := decoded[s.id]
		if len(elems) == 0 {
			continue
		}
		if len(out) == 0 || !out[len(out)-1].Day.Equal(s.day) {
			out = append(out, DayQueryAgg{Day: s.day})
		}
		for _, e := range elems {
			k := key{null: e.qid == nil}
			if e.qid != nil {
				k.qid = *e.qid
			}
			p, seen := prev[k]
			prev[k] = e.calls
			switch {
			case !seen || p == nil || e.calls == nil:
				continue
			case *e.calls >= *p:
				out[len(out)-1].TotalCalls += float64(*e.calls - *p)
			default:
				out[len(out)-1].TotalCalls += float64(*e.calls)
			}
		}
	}
	return out
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

// seqAggs reads each day's last non-empty 'sequences' snapshot: a
// sequence's use only grows (a restart is a new, lower reading), so the
// day's last reading is its use that day.
func (h *dayHistory) seqAggs(ctx context.Context, pool *pgxpool.Pool,
	days int) ([]DaySeqAgg, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, last, err := dayPicks(ctx, pool, "sequences", days)
	if err != nil {
		return nil, err
	}
	cached := keepOnly(last, h.seqs)
	err = decodeBatches(missingIDs(last, cached), func(ids []int64) error {
		return decodeSequenceUse(ctx, pool, ids, cached)
	})
	if err != nil {
		return nil, err
	}
	h.seqs = cached
	var out []DaySeqAgg
	for _, s := range last {
		out = append(out, daySequences(s.day, cached[s.id])...)
	}
	return out, nil
}

// daySequences is one day's maximum use per sequence, ordered by name.
func daySequences(day time.Time, elems []sequenceUse) []DaySeqAgg {
	byName := map[string]*DaySeqAgg{}
	var names []string
	for _, e := range elems {
		a, ok := byName[e.name]
		if !ok {
			byName[e.name] = &DaySeqAgg{Day: day, SeqName: e.name, PctUsed: e.pct,
				MaxValue: e.maxValue}
			names = append(names, e.name)
			continue
		}
		a.PctUsed, a.MaxValue = max(a.PctUsed, e.pct), max(a.MaxValue, e.maxValue)
	}
	sort.Strings(names)
	out := make([]DaySeqAgg, 0, len(names))
	for _, n := range names {
		out = append(out, *byName[n])
	}
	return out
}

func decodeSequenceUse(ctx context.Context, pool *pgxpool.Pool, ids []int64,
	into map[int64][]sequenceUse) error {
	for _, id := range ids {
		into[id] = nil
	}
	rows, err := histstore.Resolve(pool).Query(ctx, sequenceUseSQL, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name *string
		var pct *float64
		var maxValue *int64
		if err := rows.Scan(&id, &name, &pct, &maxValue); err != nil {
			return fmt.Errorf("scan sequence sample: %w", err)
		}
		if name == nil || pct == nil {
			continue // an element without a name or use is no sequence reading
		}
		e := sequenceUse{name: *name, pct: *pct}
		if maxValue != nil {
			e.maxValue = *maxValue
		}
		into[id] = append(into[id], e)
	}
	return rows.Err()
}
