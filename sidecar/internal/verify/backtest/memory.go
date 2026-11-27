package backtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Interval is one collector cycle of one statement: its counter deltas
// over the cycle ending at To.
type Interval struct {
	To     time.Time
	Calls  int64
	Ms     float64
	Blocks int64
	Rows   int64
}

// Memory is a History held in memory: the bench fixtures, and the
// in-memory side of the integration test. It reads exactly as verify's
// CounterSource reads snapshots and the hourly rollup: a cycle belongs to
// the bucket and the hour its end falls in, and a cycle ending on an epoch
// break counts nothing and flags its window and its hour. Safe for
// concurrent readers once built.
type Memory struct {
	Name   string
	Stmts  []Statement
	Acts   []Action
	ivs    map[int64][]Interval
	breaks []time.Time
	first  time.Time
	last   time.Time
}

// NewMemory holds ivs (per queryid, any order) of a history whose first
// collection was at start, with epoch breaks at the cycle ends in breaks.
func NewMemory(name string, start time.Time, stmts []Statement, acts []Action,
	ivs map[int64][]Interval, breaks []time.Time) *Memory {
	m := &Memory{Name: name, Stmts: stmts, Acts: acts, ivs: map[int64][]Interval{},
		breaks: slices.Clone(breaks), first: start.UTC()}
	slices.SortFunc(m.breaks, time.Time.Compare)
	for id, list := range ivs {
		list = slices.Clone(list)
		slices.SortFunc(list, func(a, b Interval) int { return a.To.Compare(b.To) })
		m.ivs[id] = list
		if len(list) > 0 && list[len(list)-1].To.After(m.last) {
			m.last = list[len(list)-1].To
		}
	}
	return m
}

// Intervals are queryid's cycles, oldest first.
func (m *Memory) Intervals(id int64) []Interval { return m.ivs[id] }

// Breaks are the cycle ends with an epoch break.
func (m *Memory) Breaks() []time.Time { return slices.Clone(m.breaks) }

// Span is the first collection and the last cycle end; none without a
// cycle.
func (m *Memory) Span(context.Context) (time.Time, time.Time, bool, error) {
	return m.first, m.last, !m.last.IsZero(), nil
}

// Statements are the fixture's statements.
func (m *Memory) Statements(context.Context, time.Time) ([]Statement, error) {
	return slices.Clone(m.Stmts), nil
}

// Actions are the fixture's actions in [from, to).
func (m *Memory) Actions(_ context.Context, from, to time.Time) ([]Action, error) {
	var out []Action
	for _, a := range m.Acts {
		if !a.At.Before(from) && a.At.Before(to) {
			out = append(out, a)
		}
	}
	return out, nil
}

// in is the cycles of list ending in [from, to).
func in(list []Interval, from, to time.Time) []Interval {
	lo := sort.Search(len(list), func(i int) bool { return !list[i].To.Before(from) })
	hi := sort.Search(len(list), func(i int) bool { return !list[i].To.Before(to) })
	return list[lo:hi]
}

func (m *Memory) broken(from, to time.Time) bool {
	i := sort.Search(len(m.breaks), func(i int) bool { return !m.breaks[i].Before(from) })
	return i < len(m.breaks) && m.breaks[i].Before(to)
}

func (m *Memory) idsOr(ids []int64) []int64 {
	if len(ids) > 0 {
		return ids
	}
	out := make([]int64, 0, len(m.ivs))
	for id := range m.ivs {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Window buckets the cycles ending in [from, to) as CounterSource does.
func (m *Memory) Window(_ context.Context, ids []int64, from, to time.Time,
	width time.Duration) (verify.CounterWindow, error) {
	if !to.After(from) {
		return verify.CounterWindow{}, fmt.Errorf("window %s..%s is empty", from, to)
	}
	if width <= 0 {
		width = verify.BucketWidth(to.Sub(from))
	}
	out := verify.CounterWindow{Series: verify.Series{}}
	for _, id := range m.idsOr(ids) {
		var buckets []verify.Bucket
		for _, iv := range in(m.ivs[id], from, to) {
			if m.broken(iv.To, iv.To.Add(time.Nanosecond)) {
				out.EpochBreak = true
				continue
			}
			start := from.Add(iv.To.Sub(from) / width * width).UTC()
			if n := len(buckets); n == 0 || !buckets[n-1].Start.Equal(start) {
				buckets = append(buckets, verify.Bucket{Start: start})
			}
			addTo(&buckets[len(buckets)-1], iv)
		}
		if len(buckets) > 0 {
			out.Series[id] = buckets
		}
	}
	return out, nil
}

func addTo(b *verify.Bucket, iv Interval) {
	b.Calls += iv.Calls
	b.TotalMs += iv.Ms
	b.Blocks += float64(iv.Blocks)
	b.Rows += iv.Rows
}

// Seasonal sums the cycles of the hours of [from, to) shifted back 1..weeks
// weeks, as the hourly rollup does; an hour with a break is left out.
func (m *Memory) Seasonal(_ context.Context, ids []int64, from, to time.Time,
	weeks int) (verify.Series, error) {
	if !to.After(from) {
		return nil, errors.New("seasonal window is empty")
	}
	if len(ids) == 0 {
		return verify.Series{}, nil
	}
	if weeks <= 0 {
		weeks = baseline.DefaultWeeks
	}
	var hours []time.Time
	for h := from.UTC().Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		for k := 1; k <= weeks; k++ {
			hours = append(hours, h.Add(-time.Duration(k)*7*day))
		}
	}
	slices.SortFunc(hours, time.Time.Compare)
	hours = slices.Compact(hours)
	out := verify.Series{}
	for _, id := range ids {
		for _, h := range hours {
			cycles := in(m.ivs[id], h, h.Add(time.Hour))
			if len(cycles) == 0 || m.broken(h, h.Add(time.Hour)) {
				continue
			}
			b := verify.Bucket{Start: h}
			for _, iv := range cycles {
				addTo(&b, iv)
			}
			out[id] = append(out[id], b)
		}
	}
	return out, nil
}
