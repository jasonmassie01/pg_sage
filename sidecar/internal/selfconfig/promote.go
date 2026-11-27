package selfconfig

import (
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// StepInput is everything one derivation step of one key needs.
type StepInput struct {
	Rule    Rule
	Restart bool // the key is restart-bound (lifecycle restart)
	Phase   Phase
	Now     time.Time
	Soak    time.Duration
	Prev    State
	// Proposal is this pass's candidate (OK false: none, with why).
	Proposal Proposal
	// Sample is this pass's soak observation (nil: none).
	Sample *float64
	// Running is the value the runtime config holds now.
	Running float64
	// Default is the value in force without a promoted derivation (the
	// runtime config's own value; derivation never writes it back).
	Default float64
	// OperatorSet: the operator set the key, to Operator.
	OperatorSet bool
	Operator    float64
	Cfg         *config.Config
}

// StepResult is the next state, the ledger events and whether the
// runtime config must be written (Next.Value).
type StepResult struct {
	Next   State
	Events []Event
	Apply  bool
}

type stepper struct {
	in    StepInput
	next  State
	evs   []Event
	apply bool
}

// Step advances one key. The operator's value always wins and is never
// written by derivation; a pin holds the pinned value; otherwise a
// candidate different from the active value enters shadow, gathers soak
// samples and is promoted once the soak has passed and the comparison is
// not worse. A restart-bound key changes only at startup: while running
// it keeps the value it started with (restored if a reload reset it) and
// a newer value is reported pending.
func Step(in StepInput) StepResult {
	s := &stepper{in: in, next: copyState(in.Prev)}
	s.next.Key, s.next.Rule, s.next.RuleVersion = in.Rule.Key, in.Rule.Name, in.Rule.Version
	s.next.Note = ""
	if in.Proposal.OK || len(in.Proposal.Evidence) > 0 {
		s.next.Evidence = in.Proposal.Evidence
	}
	if in.Proposal.OK || in.Proposal.Bounds != (Bounds{}) {
		s.next.Bounds = in.Proposal.Bounds
	}
	if !in.Proposal.OK {
		s.next.Note = in.Proposal.Reason
	}
	s.dropOtherVersion()
	s.clampActive()
	if in.OperatorSet {
		s.operator()
		return s.result()
	}
	if in.Prev.Status == StatusOperator || in.Prev.Operator != nil {
		s.next.Operator = nil
		s.emit(EventResumed, nil, in.Prev.Operator,
			"the operator no longer sets this key; derivation resumes")
	}
	if s.next.Pinned != nil {
		s.next.clearShadow()
	} else if in.Proposal.OK {
		s.candidate()
	}
	s.settle()
	return s.result()
}

func (s *stepper) result() StepResult {
	s.next.Status = s.next.status()
	s.next.UpdatedAt = s.in.Now
	return StepResult{Next: s.next, Events: s.evs, Apply: s.apply}
}

func (s *stepper) emit(kind EventKind, value, previous *float64, reason string) {
	s.evs = append(s.evs, Event{Kind: kind, Value: value, Previous: previous, Reason: reason})
}

func (s *stepper) previousValue() *float64 {
	if s.in.Prev.Key == "" {
		return ptr(s.in.Running)
	}
	return ptr(s.in.Prev.Value)
}

func (s *stepper) operator() {
	op := s.in.Operator
	if s.in.Prev.Status != StatusOperator {
		s.emit(EventOperatorSet, ptr(op), s.previousValue(),
			"the operator set this key; the operator's value always wins")
	}
	s.next.Operator = ptr(op)
	s.next.clearShadow()
	s.next.Value, s.next.Pending = s.in.Running, nil
	if !equal(s.in.Running, op) {
		s.next.Pending = ptr(op) // the config system has not applied it yet
	}
}

func (s *stepper) current() float64 {
	if s.next.Active != nil {
		return *s.next.Active
	}
	return s.in.Default
}

// candidate moves the shadow on this pass's candidate with hysteresis: a
// candidate within one step of the shadow, on the same side of the value
// in force, is noise and keeps the soak; two or more steps or a change of
// side restart it. A candidate within one step of a derived value in force
// starts no shadow (the promoted value already absorbed that noise). At
// the default, a return to it is noise for a one-step shadow (see
// boundaryNoise).
func (s *stepper) candidate() {
	cand, current, shadow := s.in.Proposal.Value, s.current(), s.next.Shadow
	switch {
	case equal(cand, current) && shadow != nil && s.boundaryNoise(*shadow, current):
		s.soak(current, *shadow)
	case equal(cand, current):
		s.clear(current, "the evidence supports the value in force again")
	case shadow != nil && s.keepsShadow(cand, *shadow, current):
		s.soak(current, *shadow)
	case s.next.Active != nil && s.withinStep(cand, current):
		s.clear(current, "the evidence is back within one step of the derived value in force")
	default:
		s.emit(EventShadow, ptr(cand), ptr(current), s.in.Proposal.Reason)
		s.next.clearShadow()
		s.next.Shadow, s.next.ShadowSince = ptr(cand), s.in.Now
		s.next.Derivations = appendCapped(nil, s.in.Proposal.Raw)
	}
}

// keepsShadow: cand is within one step of shadow, on the same side of the
// value in force.
func (s *stepper) keepsShadow(cand, shadow, current float64) bool {
	return s.withinStep(cand, shadow) && (cand > current) == (shadow > current)
}

func (s *stepper) withinStep(a, b float64) bool {
	return math.Abs(a-b) <= s.in.Rule.Step+1e-9
}

// clear drops the shadow (if any) with a ledger row.
func (s *stepper) clear(current float64, reason string) {
	if s.next.Shadow == nil {
		return
	}
	s.emit(EventCleared, ptr(current), ptr(*s.next.Shadow), reason)
	s.next.clearShadow()
}

// soak records this pass and, once the soak has passed, promotes the
// shadow (a cost-like rule: the p90 of its derivations) when the
// comparison is not worse. A hold is recorded only when its outcome
// changes; the latest reason stays on the state.
func (s *stepper) soak(current, shadow float64) {
	s.next.Samples = appendCapped(s.next.Samples, s.in.Sample)
	s.next.Derivations = appendCapped(s.next.Derivations, s.in.Proposal.Raw)
	if s.in.Now.Sub(s.next.ShadowSince) < s.in.Soak {
		return
	}
	if s.soakSupports(current) {
		s.clear(current, "over the soak the evidence supports the value in force")
		return
	}
	target := s.promotion(shadow)
	v := s.in.Rule.Judge(current, target, s.next.Samples, s.in.Cfg)
	if v.Outcome == OutcomeNotWorse {
		reason := v.Reason
		if s.in.Restart && s.in.Phase != PhaseStartup {
			reason += "; restart-bound, takes effect at the next restart"
		}
		s.emit(EventPromoted, ptr(target), ptr(current), reason)
		s.next.Active = ptr(target)
		s.next.clearShadow()
		return
	}
	if v.Outcome != s.next.ShadowOutcome {
		s.emit(EventHeld, ptr(target), ptr(current), v.Reason)
	}
	s.next.ShadowOutcome, s.next.ShadowReason = v.Outcome, v.Reason
}

// promotion is the value a passed soak promotes: the shadow, or for a
// cost-like rule the p90 of the soak's derivations rounded up to the step
// (one cheap pass does not set the cadence); clamped to the bounds.
func (s *stepper) promotion(shadow float64) float64 {
	if p, ok := percentile90(s.next.Derivations); ok && s.in.Rule.PromoteP90 {
		return s.candidateOf(p)
	}
	return s.candidateOf(shadow)
}

// appendCapped appends *v (nil: nothing), keeping the newest MaxSoakSamples.
func appendCapped(xs []float64, v *float64) []float64 {
	if v == nil {
		return xs
	}
	xs = append(xs, *v)
	if n := len(xs); n > MaxSoakSamples {
		xs = append([]float64(nil), xs[n-MaxSoakSamples:]...)
	}
	return xs
}

// settle decides the value in force and whether the runtime is written.
func (s *stepper) settle() {
	want := s.current()
	if s.next.Pinned != nil {
		want = *s.next.Pinned
	}
	if !s.in.Restart || s.in.Phase == PhaseStartup {
		prev := s.in.Prev
		if s.in.Phase == PhaseStartup && prev.Pending != nil && equal(*prev.Pending, want) {
			s.emit(EventApplied, ptr(want), ptr(prev.Value),
				"the restart-bound value took effect at startup")
		}
		s.next.Value, s.next.Pending = want, nil
		s.apply = !equal(s.in.Running, want)
		return
	}
	target := s.in.Running
	if s.in.Prev.Key != "" {
		target = s.in.Prev.Value // the value this process started with
	}
	s.next.Value, s.next.Pending = target, nil
	s.apply = !equal(s.in.Running, target)
	if !equal(want, target) {
		s.next.Pending = ptr(want)
	}
}

func copyState(in State) State {
	out := in
	out.Pending = copyPtr(in.Pending)
	out.Active = copyPtr(in.Active)
	out.Shadow = copyPtr(in.Shadow)
	out.Pinned = copyPtr(in.Pinned)
	out.Operator = copyPtr(in.Operator)
	out.Samples = append([]float64(nil), in.Samples...)
	out.Derivations = append([]float64(nil), in.Derivations...)
	out.Evidence = append([]Citation(nil), in.Evidence...)
	return out
}

func copyPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	return ptr(*p)
}
