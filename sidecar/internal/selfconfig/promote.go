package selfconfig

import (
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

func (s *stepper) candidate() {
	cand, current := s.in.Proposal.Value, s.current()
	switch {
	case equal(cand, current):
		if s.next.Shadow != nil {
			s.emit(EventCleared, ptr(current), ptr(*s.next.Shadow),
				"the evidence supports the value in force again")
			s.next.clearShadow()
		}
	case s.next.Shadow == nil || !equal(*s.next.Shadow, cand):
		s.emit(EventShadow, ptr(cand), ptr(current), s.in.Proposal.Reason)
		s.next.clearShadow()
		s.next.Shadow, s.next.ShadowSince = ptr(cand), s.in.Now
	default:
		s.soak(current, cand)
	}
}

func (s *stepper) soak(current, cand float64) {
	if s.in.Sample != nil {
		s.next.Samples = append(s.next.Samples, *s.in.Sample)
		if n := len(s.next.Samples); n > MaxSoakSamples {
			s.next.Samples = append([]float64(nil), s.next.Samples[n-MaxSoakSamples:]...)
		}
	}
	if s.in.Now.Sub(s.next.ShadowSince) < s.in.Soak {
		return
	}
	v := s.in.Rule.Judge(current, cand, s.next.Samples, s.in.Cfg)
	if v.Outcome == OutcomeNotWorse {
		reason := v.Reason
		if s.in.Restart && s.in.Phase != PhaseStartup {
			reason += "; restart-bound, takes effect at the next restart"
		}
		s.emit(EventPromoted, ptr(cand), ptr(current), reason)
		s.next.Active = ptr(cand)
		s.next.clearShadow()
		return
	}
	if v.Outcome != s.next.ShadowOutcome ||
		(v.Outcome == OutcomeWorse && v.Reason != s.next.ShadowReason) {
		s.emit(EventHeld, ptr(cand), ptr(current), v.Reason)
	}
	s.next.ShadowOutcome, s.next.ShadowReason = v.Outcome, v.Reason
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
	out.Evidence = append([]Citation(nil), in.Evidence...)
	return out
}

func copyPtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	return ptr(*p)
}
