package slo

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

// PromSource reads an app SLI's windows from the Prometheus connector:
// the objective's queries with $window replaced by the window. Without a
// client every window is unknown (connector_not_configured).
type PromSource struct {
	Client *PromClient
}

func substitute(q string, d time.Duration) string {
	return strings.ReplaceAll(q, "$window", FormatWindow(d))
}

// Window evaluates the SLI over the d before now. Prometheus counters
// handle resets themselves (increase); the optional resets query only
// reports them. Coverage is taken as complete.
func (s PromSource) Window(ctx context.Context, o Objective, d time.Duration,
	now time.Time) Window {
	w := Window{Duration: d, Coverage: 1, LatestAt: now}
	if s.Client == nil {
		w.Unknown = ReasonConnectorNotConfigured
		return w
	}
	var err error
	if w.Eligible, err = s.Client.Query(ctx, substitute(o.EligibleQuery, d), now); err != nil {
		w.Unknown = promReason(err)
		return w
	}
	if w.Bad, err = s.Client.Query(ctx, substitute(o.BadQuery, d), now); err != nil {
		w.Unknown = promReason(err)
		return w
	}
	if o.ResetsQuery != "" {
		resets, err := s.Client.Query(ctx, substitute(o.ResetsQuery, d), now)
		if err != nil {
			w.Unknown = promReason(err)
			return w
		}
		w.Resets = int(resets)
	}
	return classify(w, o)
}

// promReason maps a connector error onto an unknown reason.
func promReason(err error) string {
	switch {
	case errors.Is(err, ErrNoData):
		return ReasonNoData
	case errors.Is(err, ErrAmbiguous):
		return ReasonAmbiguous
	case errors.Is(err, ErrInvalidValue):
		return ReasonInvalidValue
	}
	return ReasonSourceError
}

// Slices returns one recovery slice per step ending in (start, end]:
// each point of the eligible range query is a slice; a missing or
// invalid bad (or resets) point makes that slice unknown. A failing
// eligible query yields a single unknown slice with the reason.
func (s PromSource) Slices(ctx context.Context, o Objective, start, end time.Time,
	step time.Duration) []Slice {
	fail := func(reason string) []Slice {
		return []Slice{{Start: start, End: end, Unknown: reason}}
	}
	if s.Client == nil {
		return fail(ReasonConnectorNotConfigured)
	}
	eligible, err := s.Client.QueryRange(ctx, substitute(o.EligibleQuery, step), start, end, step)
	if err != nil {
		return fail(promReason(err))
	}
	bad, badErr := s.rangeByTime(ctx, o.BadQuery, start, end, step)
	resets, resetsErr := map[int64]float64{}, error(nil)
	if o.ResetsQuery != "" {
		resets, resetsErr = s.rangeByTime(ctx, o.ResetsQuery, start, end, step)
	}
	out := make([]Slice, 0, len(eligible))
	for _, p := range eligible {
		sl := Slice{Start: p.At.Add(-step), End: p.At, Eligible: p.Value}
		b, haveBad := bad[p.At.Unix()]
		r, haveResets := resets[p.At.Unix()]
		sl.Bad, sl.Resets = b, int(r)
		switch {
		case math.IsNaN(p.Value) || (haveBad && math.IsNaN(b)):
			sl.Unknown = ReasonInvalidValue
		case badErr != nil:
			sl.Unknown = promReason(badErr)
		case !haveBad:
			sl.Unknown = ReasonNoData
		case resetsErr != nil || (o.ResetsQuery != "" && !haveResets):
			sl.Unknown = ReasonNoData
		}
		out = append(out, sl)
	}
	return out
}

func (s PromSource) rangeByTime(ctx context.Context, q string, start, end time.Time,
	step time.Duration) (map[int64]float64, error) {
	pts, err := s.Client.QueryRange(ctx, substitute(q, step), start, end, step)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]float64, len(pts))
	for _, p := range pts {
		out[p.At.Unix()] = p.Value
	}
	return out, nil
}
