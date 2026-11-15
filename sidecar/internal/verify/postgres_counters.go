package verify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// CounterSource reads the attribution's counters (lab-01a) from the
// database's history store (histstore.Resolve, like
// PostgresObservationSource): a window's per-bucket calls, time, shared
// blocks and rows from the snapshot queries category, through evid-01a's
// querystore.SnapshotIntervals (one reader of snapshots, not two), and the
// seasonal history from the hourly rollup.
type CounterSource struct {
	queryer rowQuerier
}

func NewCounterSource(pool *pgxpool.Pool) *CounterSource {
	if pool == nil {
		return &CounterSource{}
	}
	return &CounterSource{queryer: pool}
}

// CounterWindow is a window's buckets per queryid. EpochBreak reports a
// statistics epoch change inside the window (a reset, or a Neon or Lakebase
// suspend and resume): the window gives no measurement.
type CounterWindow struct {
	Series     Series
	EpochBreak bool
}

func (s *CounterSource) check(from, to time.Time) error {
	if s == nil || s.queryer == nil {
		return errors.New("verify counter source pool is unavailable")
	}
	if !to.After(from) {
		return fmt.Errorf("verify counter window [%s, %s) is empty or inverted",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	return nil
}

// Window sums the snapshot intervals of ids (all when empty) ending in
// [from, to) into buckets of width from from (bucketWidth of the window
// when width is not positive).
func (s *CounterSource) Window(ctx context.Context, ids []int64, from, to time.Time,
	width time.Duration) (CounterWindow, error) {
	if err := s.check(from, to); err != nil {
		return CounterWindow{}, err
	}
	if width <= 0 {
		width = bucketWidth(to.Sub(from))
	}
	ivs, err := querystore.SnapshotIntervals(ctx, s.queryer, from, to, ids)
	if err != nil {
		return CounterWindow{}, fmt.Errorf("read query counters %s..%s: %w",
			from.Format(time.RFC3339), to.Format(time.RFC3339), err)
	}
	out := CounterWindow{Series: Series{}}
	byKey := map[int64]map[time.Time]*Bucket{}
	for _, iv := range ivs {
		if iv.EpochBreak {
			out.EpochBreak = true
			continue
		}
		start := from.Add(iv.To.Sub(from) / width * width).UTC()
		if byKey[iv.QueryID] == nil {
			byKey[iv.QueryID] = map[time.Time]*Bucket{}
		}
		b := byKey[iv.QueryID][start]
		if b == nil {
			b = &Bucket{Start: start}
			byKey[iv.QueryID][start] = b
		}
		b.Calls += iv.Calls
		b.TotalMs += iv.ExecMs
		b.Blocks += float64(iv.SharedBlksHit + iv.SharedBlksRead)
		b.Rows += iv.Rows
	}
	for id, buckets := range byKey {
		for _, b := range buckets {
			out.Series[id] = append(out.Series[id], *b)
		}
		slices.SortFunc(out.Series[id], func(a, b Bucket) int { return a.Start.Compare(b.Start) })
	}
	return out, nil
}

// seasonalSQL reads the hourly rollup rows of queryids $1 at hours $2.
// An hour with an epoch break holds a partial count and is left out.
const seasonalSQL = `/* pg_sage */ SELECT h.queryid, h.hour, h.calls, h.exec_ms,
	h.shared_blks_hit + h.shared_blks_read, h.rows
FROM sage.query_rollup_hourly h
WHERE {db:h} AND h.queryid = ANY($1::int8[]) AND h.hour = ANY($2::timestamptz[])
	AND NOT h.epoch_break
ORDER BY h.queryid, h.hour`

// Seasonal reads, for ids, the hourly rollup rows at the hours of
// [from, to) shifted back 1..weeks weeks (baseline.DefaultWeeks when weeks
// is not positive): the same hour-of-week of earlier weeks.
func (s *CounterSource) Seasonal(ctx context.Context, ids []int64, from, to time.Time,
	weeks int) (Series, error) {
	if err := s.check(from, to); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return Series{}, nil
	}
	if weeks <= 0 {
		weeks = baseline.DefaultWeeks
	}
	var hours []time.Time
	for h := from.UTC().Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		for k := 1; k <= weeks; k++ {
			hours = append(hours, h.Add(-time.Duration(k)*week))
		}
	}
	rows, err := histstore.Resolve(s.queryer).Query(ctx, seasonalSQL, ids, hours)
	if err != nil {
		return nil, fmt.Errorf("read seasonal query history: %w", err)
	}
	out, err := collectHourly(rows)
	if err != nil {
		return nil, fmt.Errorf("read seasonal query history: %w", err)
	}
	return out, nil
}

// collectHourly reads rollup rows (queryid, hour, calls, exec_ms, blocks,
// rows) into a Series.
func collectHourly(rows pgx.Rows) (Series, error) {
	type row struct {
		id int64
		b  Bucket
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		var blocks int64
		err := r.Scan(&x.id, &x.b.Start, &x.b.Calls, &x.b.TotalMs, &blocks, &x.b.Rows)
		x.b.Start, x.b.Blocks = x.b.Start.UTC(), float64(blocks)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := Series{}
	for _, x := range got {
		out[x.id] = append(out[x.id], x.b)
	}
	return out, nil
}

// hourlySQL reads the hourly rollup rows of queryids $1 in [$2, $3),
// leaving out epoch-broken hours (partial counts).
const hourlySQL = `/* pg_sage */ SELECT h.queryid, h.hour, h.calls, h.exec_ms,
	h.shared_blks_hit + h.shared_blks_read, h.rows
FROM sage.query_rollup_hourly h
WHERE {db:h} AND h.queryid = ANY($1::int8[]) AND h.hour >= $2 AND h.hour < $3
	AND NOT h.epoch_break
ORDER BY h.queryid, h.hour`

// Hourly reads, for ids, the hourly rollup rows in [from, to): the history
// PreflightVerifiable judges a step's targets by (lab-01c).
func (s *CounterSource) Hourly(ctx context.Context, ids []int64, from, to time.Time) (Series,
	error) {
	if err := s.check(from, to); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return Series{}, nil
	}
	rows, err := histstore.Resolve(s.queryer).Query(ctx, hourlySQL, ids, from, to)
	if err != nil {
		return nil, fmt.Errorf("read hourly query history: %w", err)
	}
	out, err := collectHourly(rows)
	if err != nil {
		return nil, fmt.Errorf("read hourly query history: %w", err)
	}
	return out, nil
}
