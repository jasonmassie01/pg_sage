package verify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Verification v2 in the index watch (lab-01c). With v2 enabled the
// engine computes v2 beside v1 at every look: in shadow it acts on v1 and
// stores v2's verdict as the other one; in v2 it acts on v2 and stores
// v1's. An invalid index is reverted whatever the method. The look's
// alpha spend and the other verdict are written by the same Update as the
// acted-on verdict, so a failed write leaves the watch as stored.

// V2Counters reads the attribution's counters (CounterSource).
type V2Counters interface {
	Window(ctx context.Context, ids []int64, from, to time.Time, width time.Duration) (
		CounterWindow, error)
	Seasonal(ctx context.Context, ids []int64, from, to time.Time, weeks int) (Series, error)
}

// V2Mechanisms applies the mechanism gate (PostgresObservationSource).
type V2Mechanisms interface {
	CheckMechanism(ctx context.Context, in MechanismInput, to time.Time) (Mechanism, error)
}

// V2Options wires v2 into an engine. Method is the resolved method of the
// engine's database (v1, shadow or v2; anything else is v1), read at each
// look. Controls picks a watch's control statements (nil: none, so the
// attribution has no control term). Cycle is the collector cycle.
type V2Options struct {
	Counters   V2Counters
	Mechanisms V2Mechanisms
	Method     func(context.Context) string
	Controls   func(context.Context, WatchState) ([]int64, error)
	Cycle      time.Duration
}

// defaultV2Cycle is the collector cycle when none is configured.
const defaultV2Cycle = time.Minute

// EnableV2 wires v2 into the engine. It refuses missing counters, a
// missing mechanism source and a missing method.
func (e *Engine) EnableV2(o V2Options) error {
	if o.Counters == nil || o.Mechanisms == nil || o.Method == nil {
		return errors.New("verify: verification v2 needs counters, a mechanism source " +
			"and a method")
	}
	if o.Cycle <= 0 {
		o.Cycle = defaultV2Cycle
	}
	e.v2 = &o
	return nil
}

func (e *Engine) method(ctx context.Context) string {
	if e.v2 == nil {
		return MethodV1
	}
	switch m := e.v2.Method(ctx); m {
	case MethodShadow, MethodV2:
		return m
	}
	return MethodV1
}

// freezeWeights stores the targets' pre-period call shares with a new
// watch (lab-01a's frozen weights). A failed read leaves them nil: the
// attribution then takes them from each look's pre-window.
func (e *Engine) freezeWeights(ctx context.Context, state *WatchState) {
	if state.FrozenWeights != nil || e.method(ctx) == MethodV1 {
		return
	}
	targets := state.Criterion.TargetIDs
	pre, err := e.v2.Counters.Window(ctx, targets, state.ExecutedAt.Add(-state.Window),
		state.ExecutedAt, 0)
	if err == nil {
		if w := FrozenWeights(pre.Series, targets); len(w) > 0 {
			state.FrozenWeights = w
		}
	}
}

// withV2 adds v2 to v1's verdict as the method says.
func (e *Engine) withV2(ctx context.Context, state WatchState, observed observations,
	v1 Verdict) Verdict {
	v1.Verifier = VerifierV1
	method := e.method(ctx)
	if method == MethodV1 {
		return v1
	}
	v2, spend, err := e.judgeV2(ctx, state)
	if err != nil {
		if method == MethodV2 && observed.indexValid {
			retry := e.retryVerdict(ctx, state, fmt.Errorf("verification v2: %w", err))
			retry.Verifier = VerifierV2
			return retry
		}
		v2 = v2Unavailable(err)
	}
	if method == MethodShadow || !observed.indexValid {
		return actOn(v1, v2, VerifierV1, VerifierV2, spend)
	}
	return actOn(e.withWrites(state, observed, v2), v1, VerifierV2, VerifierV1, spend)
}

func v2Unavailable(err error) Verdict {
	reason := "v2 unavailable: " + err.Error()
	return Verdict{Outcome: OutcomeInsufficient, Reason: reason,
		Evidence: map[string]any{"v2": map[string]any{"verifier": VerifierV2,
			"outcome": OutcomeInsufficient, "reason": reason}}}
}

// actOn is acted with other recorded beside it; the v2 record is in the
// acted verdict's evidence either way.
func actOn(acted, other Verdict, verifier, otherVerifier string, spend *AlphaSpend) Verdict {
	acted.Verifier, acted.AlphaSpend = verifier, spend
	acted.Other = &OtherVerdict{Verifier: otherVerifier, Outcome: other.Outcome,
		Reason: other.Reason, Evidence: other.Evidence}
	if acted.Evidence == nil {
		acted.Evidence = map[string]any{}
	}
	if record, ok := other.Evidence["v2"]; ok && otherVerifier == VerifierV2 {
		acted.Evidence["v2"] = record
	}
	return acted
}

// withWrites applies v1's write-cost veto to a retained v2 verdict and
// attaches the write signal.
func (e *Engine) withWrites(state WatchState, observed observations, v Verdict) Verdict {
	decisive := v.Retain &&
		judgeObservedWrites(state.Criterion, observed).Outcome == writeRegressed
	if decisive {
		v = writeImpactVerdict(v)
	}
	v.Evidence["write_signal"] = writeSignal(state, observed, decisive)
	return v
}

// v2Reads is one look's counters and controls.
type v2Reads struct {
	controls       []int64
	pre, post      CounterWindow
	seasonal       Series
	mechanism      Mechanism
	executedAt, to time.Time
}

func (e *Engine) readV2(ctx context.Context, state WatchState) (v2Reads, error) {
	r := v2Reads{executedAt: state.ExecutedAt, to: e.options.Now()}
	targets := state.Criterion.TargetIDs
	if e.v2.Controls != nil {
		controls, err := e.v2.Controls(ctx, state)
		if err != nil {
			return r, fmt.Errorf("choose control statements: %w", err)
		}
		r.controls = slices.DeleteFunc(slices.Clone(controls),
			func(id int64) bool { return slices.Contains(targets, id) })
	}
	ids := append(slices.Clone(targets), r.controls...)
	var err error
	if r.pre, err = e.v2.Counters.Window(ctx, ids, r.executedAt.Add(-state.Window),
		r.executedAt, 0); err != nil {
		return r, fmt.Errorf("read pre-action counters: %w", err)
	}
	if r.post, err = e.v2.Counters.Window(ctx, ids, r.executedAt, r.to, 0); err != nil {
		return r, fmt.Errorf("read post-action counters: %w", err)
	}
	if r.seasonal, err = e.v2.Counters.Seasonal(ctx, ids, r.executedAt, r.to, 0); err != nil {
		return r, fmt.Errorf("read seasonal history: %w", err)
	}
	r.mechanism, err = e.v2.Mechanisms.CheckMechanism(ctx, MechanismInput{
		Class: MechanismIndexCreate, Index: state.IndexName, Targets: targets,
		ExecutedAt: r.executedAt, Cycle: e.v2.Cycle}, r.to)
	if err != nil {
		return r, fmt.Errorf("check the mechanism: %w", err)
	}
	return r, nil
}

// judgeV2 is one v2 look: the reads, the look's alpha, the attribution on
// the counted targets, the contamination scan and the verdict.
func (e *Engine) judgeV2(ctx context.Context, state WatchState) (Verdict, *AlphaSpend,
	error) {
	r, err := e.readV2(ctx, state)
	if err != nil {
		return Verdict{}, nil, err
	}
	look, spend, err := nextLook(state)
	if err != nil {
		return Verdict{}, nil, err
	}
	targets := state.Criterion.TargetIDs
	if r.mechanism.Passed() && len(r.mechanism.Counted) > 0 {
		targets = r.mechanism.Counted
	}
	in := AttributionInput{Kind: AttributionIndex, Targets: targets, Controls: r.controls,
		Pre: r.pre.Series, Post: r.post.Series, Seasonal: r.seasonal,
		Weights: state.FrozenWeights, RegressPct: state.Criterion.RegressPct,
		Alpha: testAlpha(look.Alpha), EpochBreak: r.pre.EpochBreak || r.post.EpochBreak}
	a, err := Attribute(in)
	if err != nil {
		return Verdict{}, nil, err
	}
	c, err := DetectContamination(in, state.ExecutedAt, e.v2.Cycle)
	if err != nil {
		return Verdict{}, nil, err
	}
	v := JudgeV2(state, V2Evidence{Attribution: a, Mechanism: r.mechanism,
		Contamination: c, Look: look})
	return v, &spend, nil
}

// testAlpha is the alpha a look tests at. A look that spends nothing (an
// early look whose spend underflows to 0) tests at the smallest positive
// alpha, an infinite interval, never at the default alpha that a zero
// would otherwise select.
func testAlpha(alpha float64) float64 {
	if !(alpha > 0) {
		return math.SmallestNonzeroFloat64
	}
	return alpha
}

// nextLook spends the look at the watch's current window.
func nextLook(state WatchState) (Look, AlphaSpend, error) {
	c := state.Criterion
	_, final, err := LookIndex(LookSchedule(c.Window, c.HardMax), state.Window)
	if err != nil {
		return Look{}, AlphaSpend{}, err
	}
	fraction := InformationFraction(0, 0, state.Window, c.HardMax)
	return SpendLook(state.AlphaSpend, DefaultAlpha, fraction, final)
}
