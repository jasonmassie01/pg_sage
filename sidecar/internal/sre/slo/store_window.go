package slo

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// windowPoint is one sample's running counters (nil before they existed).
type windowPoint struct {
	at      *time.Time
	bad, el *float64
	n, r    *int64
	chain   *time.Time
}

// counted reports whether the point carries running counters.
func (p windowPoint) counted() bool {
	return p.at != nil && p.bad != nil && p.el != nil && p.n != nil && p.r != nil &&
		p.chain != nil
}

// windowRow is one series' newest (e), oldest (f) and baseline (b) points.
type windowRow struct {
	series  string
	e, f, b windowPoint
}

func (p *windowPoint) targets() []any {
	return []any{&p.at, &p.bad, &p.el, &p.n, &p.r, &p.chain}
}

// aggregate sums a SLO's series (or one series) over [from, to] from the
// running counters, falling back to the raw aggregation for a series
// whose window touches samples stored without them.
func (s *Store) aggregate(ctx context.Context, dep sre.UUID, slo, series string, from,
	to time.Time, lookback time.Duration) ([]SeriesAgg, error) {
	sql := allSeriesPointsSQL
	if series != "" {
		sql = oneSeriesPointsSQL
	}
	rows, err := s.pool.Query(ctx, sql, string(dep), slo, series, from, to)
	if err != nil {
		return nil, err
	}
	var points []windowRow
	for rows.Next() {
		var w windowRow
		dest := append([]any{&w.series}, w.e.targets()...)
		dest = append(dest, w.f.targets()...)
		dest = append(dest, w.b.targets()...)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan SLI window: %w", err)
		}
		if w.b.at != nil && !w.b.at.After(from.Add(-lookback)) {
			w.b = windowPoint{} // older than the lookback: no baseline
		}
		if w.e.at != nil || w.b.at != nil {
			points = append(points, w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SeriesAgg, 0, len(points))
	for _, w := range points {
		a, ok := fromCounters(w)
		if !ok {
			raw, err := s.rawSeries(ctx, dep, slo, w.series, from, to, lookback)
			if err != nil {
				return nil, err
			}
			out = append(out, raw...)
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// fromCounters is the raw aggregation of one series computed from its
// window's points: the increases between the baseline (else the oldest
// sample) and the newest. ok is false when a point has no counters or the
// two lie on different chains.
func fromCounters(w windowRow) (SeriesAgg, bool) {
	if w.e.at == nil { // only the baseline: one sample, nothing summed
		return SeriesAgg{Series: w.series, Samples: 1, First: *w.b.at, Last: *w.b.at},
			true
	}
	base := w.f
	if w.b.at != nil {
		base = w.b
	}
	if !w.e.counted() || !base.counted() || !w.e.chain.Equal(*base.chain) {
		return SeriesAgg{}, false
	}
	return SeriesAgg{Series: w.series, Samples: int(*w.e.n - *base.n + 1),
		First: *base.at, Last: *w.e.at, Bad: *w.e.bad - *base.bad,
		Eligible: *w.e.el - *base.el, Resets: int(*w.e.r - *base.r)}, true
}

// rawSeries is the raw aggregation of one series.
func (s *Store) rawSeries(ctx context.Context, dep sre.UUID, slo, series string, from,
	to time.Time, lookback time.Duration) ([]SeriesAgg, error) {
	rows, err := s.pool.Query(ctx, rawSeriesSQL, string(dep), slo, series, from, to,
		lookback.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesAgg
	for rows.Next() {
		var a SeriesAgg
		if err := rows.Scan(&a.Series, &a.Samples, &a.First, &a.Last, &a.Bad, &a.Eligible,
			&a.Resets); err != nil {
			return nil, fmt.Errorf("scan raw SLI aggregate: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
