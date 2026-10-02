package slo

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Recovery slicing.
const (
	// RecoveryStep is one recovery slice.
	RecoveryStep = 2 * time.Minute
	// maxRecoverySlices bounds the slices read (the newest hour).
	maxRecoverySlices = 30
	// baselineSpan is the pre-incident traffic window.
	baselineSpan = time.Hour
)

// RecoveryPredicate is the SLI recovery predicate an investigation's
// recovery check (or an operator) asks after an intervention.
type RecoveryPredicate interface {
	Recovery(ctx context.Context, name string, since time.Time) (Recovery, error)
}

var _ RecoveryPredicate = (*Engine)(nil)

// Recovery evaluates the SLI since an intervention: consecutive 2-minute
// slices, the newest three of which must each have trustworthy data and
// burn under 1x. Pre-incident traffic (the hour before since) is the
// baseline a recovered slice must keep at least half of.
func (e *Engine) Recovery(ctx context.Context, name string, since time.Time) (Recovery,
	error) {
	o, ok := e.objective(name)
	if !ok {
		return Recovery{}, fmt.Errorf("%w: %q", ErrUnknownSLO, name)
	}
	scope, err := e.scope(ctx)
	if err != nil {
		return Recovery{}, err
	}
	now := e.now()
	var slices []Slice
	if o.Source == SourcePrometheus {
		start := since.Add(RecoveryStep)
		if oldest := now.Add(-maxRecoverySlices * RecoveryStep); start.Before(oldest) {
			start = oldest
		}
		if now.After(start) {
			slices = e.prom.Slices(ctx, o, start, now, RecoveryStep)
		}
	} else {
		slices = e.storeSlices(ctx, scope, o, since, now)
	}
	r := EvaluateRecovery(slices, RecoveryParams{Target: o.Target, MinEligible: o.MinEligible,
		BaselineEligible: e.baselinePerSlice(ctx, scope, o, since)})
	r.Since = since
	return r, nil
}

// storeSlices reads end-aligned slices of stored counters, oldest first.
func (e *Engine) storeSlices(ctx context.Context, scope sre.Scope, o Objective, since,
	now time.Time) []Slice {
	var out []Slice
	for k := 0; k < maxRecoverySlices; k++ {
		end := now.Add(-time.Duration(k) * RecoveryStep)
		start := end.Add(-RecoveryStep)
		if start.Before(since) {
			break
		}
		w := e.storeWindow(ctx, scope, o, start, end)
		out = append(out, Slice{Start: start, End: end, Bad: w.Bad, Eligible: w.Eligible,
			Resets: w.Resets, Unknown: w.Unknown})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (e *Engine) storeWindow(ctx context.Context, scope sre.Scope, o Objective, from,
	to time.Time) Window {
	series := ""
	if o.Source == SourceProxy {
		series = string(scope.DatabaseID)
	}
	aggs, err := e.store.Aggregate(ctx, scope.DeploymentID, o.Name, series, from, to,
		o.staleAfter())
	if err != nil {
		return Window{Duration: to.Sub(from), Unknown: ReasonSourceError}
	}
	return Combine(aggs, o, from, to, to)
}

// baselinePerSlice is the pre-incident eligible events per slice, or 0
// when unknown (no baseline check then).
func (e *Engine) baselinePerSlice(ctx context.Context, scope sre.Scope, o Objective,
	since time.Time) float64 {
	var w Window
	if o.Source == SourcePrometheus {
		w = e.prom.Window(ctx, o, baselineSpan, since)
	} else {
		w = e.storeWindow(ctx, scope, o, since.Add(-baselineSpan), since)
	}
	if !w.Known() {
		return 0
	}
	return w.Eligible * float64(RecoveryStep) / float64(baselineSpan)
}
