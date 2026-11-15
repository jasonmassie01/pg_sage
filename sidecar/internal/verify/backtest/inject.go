package backtest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// memo reads one placebo's counters once: the null replay, the injected
// one and v1 read the same windows. It remembers whether any window had
// a statistics epoch break (the placebo is then excluded). It is used by
// one placebo's replay at a time and is not safe for concurrent use.
type memo struct {
	src      verify.V2Counters
	windows  map[string]verify.CounterWindow
	seasonal map[string]verify.Series
	broke    bool
}

func newMemo(src verify.V2Counters) *memo {
	return &memo{src: src, windows: map[string]verify.CounterWindow{},
		seasonal: map[string]verify.Series{}}
}

func readKey(ids []int64, from, to time.Time, n int64) string {
	return fmt.Sprint(ids, from.UnixNano(), to.UnixNano(), n)
}

func (m *memo) Window(ctx context.Context, ids []int64, from, to time.Time,
	width time.Duration) (verify.CounterWindow, error) {
	key := readKey(ids, from, to, int64(width))
	if w, ok := m.windows[key]; ok {
		return w, nil
	}
	w, err := m.src.Window(ctx, ids, from, to, width)
	if err != nil {
		return verify.CounterWindow{}, err
	}
	m.windows[key] = w
	m.broke = m.broke || w.EpochBreak
	return w, nil
}

func (m *memo) Seasonal(ctx context.Context, ids []int64, from, to time.Time,
	weeks int) (verify.Series, error) {
	key := readKey(ids, from, to, int64(weeks))
	if s, ok := m.seasonal[key]; ok {
		return s, nil
	}
	s, err := m.src.Seasonal(ctx, ids, from, to, weeks)
	if err != nil {
		return nil, err
	}
	m.seasonal[key] = s
	return s, nil
}

// injected is src with the placebo's targets InjectedDrop cheaper (blocks
// and time per call) in every bucket from the step onward: what a step
// that worked would have left in the counters. Calls are unchanged.
type injected struct {
	src     verify.V2Counters
	at      time.Time
	targets []int64
}

func (i injected) Window(ctx context.Context, ids []int64, from, to time.Time,
	width time.Duration) (verify.CounterWindow, error) {
	w, err := i.src.Window(ctx, ids, from, to, width)
	if err != nil {
		return w, err
	}
	w.Series = i.scaled(w.Series)
	return w, nil
}

func (i injected) Seasonal(ctx context.Context, ids []int64, from, to time.Time,
	weeks int) (verify.Series, error) {
	s, err := i.src.Seasonal(ctx, ids, from, to, weeks)
	if err != nil {
		return s, err
	}
	return i.scaled(s), nil
}

// scaled copies s, never changing the memo's own buckets.
func (i injected) scaled(s verify.Series) verify.Series {
	out := maps.Clone(s)
	for id, buckets := range s {
		if !slices.Contains(i.targets, id) {
			continue
		}
		bs := slices.Clone(buckets)
		for k := range bs {
			if !bs[k].Start.Before(i.at) {
				bs[k].Blocks *= 1 - InjectedDrop
				bs[k].TotalMs *= 1 - InjectedDrop
			}
		}
		out[id] = bs
	}
	return out
}
