package rollout

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Effect methods: how an instance's RegressionPct was measured (lab-03b).
const (
	// EffectFleetControl: the database's change of the target queries
	// minus the median change of the same queries on the untreated members.
	EffectFleetControl = "v2_fleet_control"
	// EffectOwnV2: the database's own v2 effect (no fleet control).
	EffectOwnV2 = "v2"
	// EffectFlagOnly: no v2 number; the verdict flag alone decided.
	EffectFlagOnly = "flag_only"
)

// minFleetControls is the fewest measured untreated members that make a
// fleet control: one database's change is its own noise, not the fleet's.
const minFleetControls = 2

// MemberEffect is a treated database's own v2 verdict of the change: its
// effect in percent of blocks per call with the v2 interval, the
// normalized texts of the change's target queries, and the verdict's
// window. Measured false: no usable number, with why (Reason); Malformed
// marks a stored v2 record that is not the shape v2 writes. Measured with
// a Reason: the number stands but its target queries could not be read.
type MemberEffect struct {
	Measured, Malformed            bool
	Reason                         string
	EffectPct, CILowPct, CIHighPct float64
	Queries                        []string
	From, To                       time.Time
}

// EffectReader is the part of a CanaryTarget that measures (lab-03b). A
// target without it is judged by its verdict flag only.
type EffectReader interface {
	ActionEffect(ctx context.Context, actionLogID int64) (MemberEffect, error)
	// QueryChange is the change in percent of the queries' blocks per call
	// over [from, to) against the as-long period before from; ran is false
	// when the database ran none of them in either period.
	QueryChange(ctx context.Context, queries []string, from, to time.Time) (pct float64,
		ran bool, err error)
}

// FleetLadderRecorder records a database's tier-4 ladder record.
type FleetLadderRecorder interface {
	RecordFleetLadder(ctx context.Context, actionLogID int64, rec verify.LadderRecord) error
}

// withEffect gives a settled outcome its measured effect: RegressionPct
// becomes the number, the flag (Failed) still halts on its own, and with
// no number the outcome is flag only, said so in the detail.
func (b *canaryBackend) withEffect(ctx context.Context, self string, target CanaryTarget,
	actionID int64, o Outcome) Outcome {
	o = b.measure(ctx, self, target, actionID, o)
	b.recordLadder(ctx, self, target, actionID, o)
	return o
}

func (b *canaryBackend) measure(ctx context.Context, self string, target CanaryTarget,
	actionID int64, o Outcome) Outcome {
	reader, ok := target.(EffectReader)
	if !ok {
		return flagOnly(o, "the database reads no v2 effect")
	}
	eff, err := reader.ActionEffect(ctx, actionID)
	switch {
	case err != nil:
		b.svc.opts.Log("rollout: %s: read the v2 effect of action %d: %v; judged by "+
			"the verdict flag only", self, actionID, err)
		return flagOnly(o, "read the v2 effect: "+err.Error())
	case eff.Malformed:
		b.svc.opts.Log("rollout: %s: action %d: %s; judged by the verdict flag only",
			self, actionID, eff.Reason)
		return flagOnly(o, eff.Reason)
	case !eff.Measured:
		return flagOnly(o, eff.Reason)
	}
	lo, hi := eff.CILowPct, eff.CIHighPct
	o.CILowPct, o.CIHighPct = &lo, &hi
	effect, control, note := b.netEffect(ctx, self, reader, eff)
	o.EffectPct, o.ControlPct, o.RegressionPct = &effect, control, effect
	o.EffectMethod = EffectOwnV2
	if control != nil {
		o.EffectMethod = EffectFleetControl
	}
	o.Detail += fmt.Sprintf("; effect %+.2f%% of blocks per call (%s; own v2 %+.2f%%, "+
		"CI %+.2f..%+.2f%%); %s", effect, o.EffectMethod, eff.EffectPct, lo, hi, note)
	return o
}

func flagOnly(o Outcome, why string) Outcome {
	o.EffectMethod = EffectFlagOnly
	o.Detail += "; flag only: " + why
	return o
}

// netEffect is the database's change net of the fleet control, or its own
// v2 effect when there is no control or its own change is unmeasured.
func (b *canaryBackend) netEffect(ctx context.Context, self string, reader EffectReader,
	eff MemberEffect) (float64, *float64, string) {
	if len(eff.Queries) == 0 {
		note := "no fleet control: no target query text on " + self
		if eff.Reason != "" {
			note += " (" + eff.Reason + ")"
		}
		return eff.EffectPct, nil, note
	}
	control, note := b.fleetControl(ctx, self, eff)
	if control == nil {
		return eff.EffectPct, nil, note
	}
	own, ran, err := reader.QueryChange(ctx, eff.Queries, eff.From, eff.To)
	switch {
	case err != nil:
		return eff.EffectPct, nil, "no fleet control: own change unread: " + err.Error()
	case !ran:
		return eff.EffectPct, nil, "no fleet control: own change unmeasured (" + self +
			" ran none of the target queries)"
	}
	return roundPct(own - *control), control, fmt.Sprintf("own change %+.2f%%, %s", own,
		note)
}

// roundPct rounds to 1e-6 points, so an effect at the halt bar compares
// with it exactly (as v2's own judge does) instead of by float noise.
func roundPct(p float64) float64 { return math.Round(p*1e6) / 1e6 }

// memberChange is one untreated member's change of the target queries.
type memberChange struct {
	name string
	pct  float64
	ran  bool
	err  error
}

// fleetControl is the median change of the target queries on the
// untreated members over the same hours, read in parallel. A member that
// cannot be read, or ran none of the queries, is left out by name; with
// fewer than minFleetControls measured there is no control.
func (b *canaryBackend) fleetControl(ctx context.Context, self string,
	eff MemberEffect) (*float64, string) {
	untreated := b.untreated(self)
	results := make([]memberChange, len(untreated))
	var wg sync.WaitGroup
	for i, name := range untreated {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = b.readChange(ctx, name, eff)
		}()
	}
	wg.Wait()
	var used, left []string
	var pcts []float64
	for _, r := range results {
		switch {
		case r.err != nil:
			left = append(left, fmt.Sprintf("%s (%v)", r.name, r.err))
		case !r.ran:
			left = append(left, r.name+" (ran none of the target queries)")
		default:
			used, pcts = append(used, r.name), append(pcts, r.pct)
		}
	}
	leftNote := ""
	if len(left) > 0 {
		leftNote = "; left out of the fleet control: " + strings.Join(left, ", ")
	}
	if len(pcts) < minFleetControls {
		return nil, fmt.Sprintf("no fleet control: %d untreated member(s) measured, "+
			"%d needed%s", len(pcts), minFleetControls, leftNote)
	}
	m := roundPct(median(pcts))
	return &m, fmt.Sprintf("fleet control %+.2f%% (median of %s)%s", m,
		strings.Join(used, ", "), leftNote)
}

func (b *canaryBackend) readChange(ctx context.Context, name string,
	eff MemberEffect) memberChange {
	target, err := b.svc.resolve(name)
	if err != nil {
		return memberChange{name: name, err: err}
	}
	reader, ok := target.(EffectReader)
	if !ok {
		return memberChange{name: name, err: fmt.Errorf("reads no query history")}
	}
	pct, ran, err := reader.QueryChange(ctx, eff.Queries, eff.From, eff.To)
	return memberChange{name: name, pct: pct, ran: ran, err: err}
}

// untreated are the run's databases other than self with no change yet.
func (b *canaryBackend) untreated(self string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, name := range b.targets {
		if _, changed := b.actions[name]; !changed && name != self {
			out = append(out, name)
		}
	}
	return out
}

func median(values []float64) float64 {
	s := slices.Clone(values)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// recordLadder sets the database's tier-4 ladder record. It is evidence
// only: a failed write is logged and never changes the rollout.
func (b *canaryBackend) recordLadder(ctx context.Context, self string, target CanaryTarget,
	actionID int64, o Outcome) {
	recorder, ok := target.(FleetLadderRecorder)
	if !ok {
		return
	}
	rec := verify.FromFleetEffect(verify.FleetEffect{Database: self, ActionID: actionID,
		EffectPct: o.EffectPct, ControlPct: o.ControlPct, Method: o.EffectMethod,
		LimitPct: b.svc.opts.RegressionLimitPct, Failed: o.Failed, Detail: o.Detail},
		b.svc.opts.Now())
	if err := recorder.RecordFleetLadder(ctx, actionID, rec); err != nil {
		b.svc.opts.Log("rollout: %s: record the tier-4 ladder of action %d: %v", self,
			actionID, err)
	}
}

// markPauseDue notes a recorded verdict: the next database waits.
func (b *canaryBackend) markPauseDue() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pauseDue = true
}

// stagePause waits StageWait before the next database after a verdict.
func (b *canaryBackend) stagePause(ctx context.Context) error {
	b.mu.Lock()
	due := b.pauseDue && b.svc.opts.StageWait > 0
	b.pauseDue = false
	b.mu.Unlock()
	if !due {
		return nil
	}
	b.svc.opts.Log("rollout: pausing %s before the next database (stage_wait_hours)",
		b.svc.opts.StageWait)
	return b.svc.wait(ctx, b.svc.opts.StageWait)
}
