package selfconfig

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// State transitions of a value in force (footprint-04): every step clamps
// the promoted value to the rule's current bounds (with a ledger row when
// that changes it), and a rule version change drops the promoted value so
// the new rule earns its own.

const seqKey = "sre.runways.sequence_interval_seconds"

// seqInput steps the (restart-bound) sequence interval at startup.
func seqInput(t *testing.T, cfg *config.Config, prev State, running float64) StepInput {
	t.Helper()
	r := rule(t, seqKey)
	ev := costEvidence(t, seqKey, 1500)
	in := StepInput{Rule: r, Restart: isRestart(r.Key), Phase: PhaseStartup, Now: t0,
		Soak: soak, Prev: prev, Proposal: r.Evaluate(ev, cfg), Running: running,
		Default: running, Cfg: cfg}
	if v, ok := r.Observe(ev, cfg); ok {
		in.Sample = &v
	}
	return in
}

func promotedSeq(v float64) State {
	return State{Key: seqKey, Status: StatusDerived, Value: v, Active: f(v),
		Rule: "sequence_sampling_by_cost", RuleVersion: 1}
}

func TestTightenedBoundsClampThePromotedValue(t *testing.T) {
	cfg := defaults()
	if res := Step(seqInput(t, cfg, promotedSeq(1500), 1500)); len(res.Events) != 0 ||
		res.Next.Value != 1500 || res.Apply {
		t.Fatalf("steady derived value changed: %+v", res)
	}
	// min_samples 31 over a 6 h lookback allows at most 720 s between samples.
	cfg.SRE.Runways.MinSamples = 31
	res := Step(seqInput(t, cfg, promotedSeq(1500), 1500))
	n := res.Next
	if n.Active == nil || *n.Active != 720 || n.Value != 720 || !res.Apply ||
		n.Status != StatusDerived || n.Shadow != nil {
		t.Fatalf("promoted value not clamped to the tightened bounds: %+v", res)
	}
	if kinds(res.Events) != "clamped" || *res.Events[0].Value != 720 ||
		*res.Events[0].Previous != 1500 ||
		!strings.Contains(res.Events[0].Reason, "sre.runways.min_samples") {
		t.Fatalf("events %+v", res.Events)
	}
	// The next pass records nothing more.
	again := Step(seqInput(t, cfg, n, 720))
	if len(again.Events) != 0 || again.Next.Value != 720 || again.Apply {
		t.Fatalf("second pass %+v", again)
	}
	// While running, the restart-bound key keeps its startup value and
	// reports the clamped one pending.
	live := seqInput(t, cfg, promotedSeq(1500), 1500)
	live.Phase = PhaseLive
	res = Step(live)
	if res.Next.Value != 1500 || res.Next.Pending == nil || *res.Next.Pending != 720 ||
		res.Apply || kinds(res.Events) != "clamped" {
		t.Fatalf("live clamp of a restart-bound key: %+v", res)
	}
}

// Empty bounds (no value can satisfy the other keys) drop the promoted
// value: the operator's configuration is in force again.
func TestEmptyBoundsDropThePromotedValue(t *testing.T) {
	cfg := defaults()
	cfg.SRE.Runways.LookbackHours, cfg.SRE.Runways.MinSamples = 1, 10 // at most 400 s
	in := seqInput(t, cfg, promotedSeq(1500), 1500)
	in.Default = 600
	res := Step(in)
	n := res.Next
	if n.Active != nil || n.Value != 600 || !res.Apply || n.Status != StatusDefault {
		t.Fatalf("promoted value kept under empty bounds: %+v", res)
	}
	if kinds(res.Events) != "cleared" || *res.Events[0].Previous != 1500 ||
		!strings.Contains(res.Events[0].Reason, "bounds") {
		t.Fatalf("events %+v", res.Events)
	}
}

func TestPinnedValueIsNotClamped(t *testing.T) {
	cfg := defaults()
	cfg.SRE.Runways.MinSamples = 31
	prev := promotedSeq(1500)
	prev.Status, prev.Pinned, prev.PinnedBy, prev.PinnedAt = StatusPinned, f(1500), "a", t0
	res := Step(seqInput(t, cfg, prev, 1500))
	n := res.Next
	if n.Status != StatusPinned || n.Value != 1500 || res.Apply || n.Active == nil ||
		*n.Active != 720 || kinds(res.Events) != "clamped" {
		t.Fatalf("pin did not win over the clamp of the promoted value: %+v", res)
	}
}

func TestRuleVersionChangeClearsTheActiveValue(t *testing.T) {
	prev := derivedState(180)
	prev.RuleVersion = 2 // the rule is now version 3
	in := baseInput(testRule(nil), prev, withRaw(proposal(180), 175), t0)
	in.Running = 180
	res := Step(in)
	n := res.Next
	if n.Active != nil || n.Value != 60 || !res.Apply || n.RuleVersion != 3 {
		t.Fatalf("version change kept the promoted value: %+v", res)
	}
	if kinds(res.Events) != "cleared,shadow" || *res.Events[0].Previous != 180 ||
		!strings.Contains(res.Events[0].Reason, "version 2") {
		t.Fatalf("events %+v", res.Events)
	}
	if n.Shadow == nil || *n.Shadow != 180 || !n.ShadowSince.Equal(t0) {
		t.Fatalf("the new rule does not soak its own candidate: %+v", n)
	}
	// Same version: nothing happens.
	prev.RuleVersion = 3
	in.Prev = prev
	if res := Step(in); len(res.Events) != 0 || res.Next.Active == nil {
		t.Fatalf("same version %+v", res)
	}
	// A state that records no version (never stored) is not cleared.
	prev.RuleVersion = 0
	in.Prev = prev
	if res := Step(in); len(res.Events) != 0 || res.Next.Active == nil {
		t.Fatalf("unrecorded version %+v", res)
	}
}

func TestRuleVersionChangeRestartsAShadow(t *testing.T) {
	prev := shadowState(120, t0)
	prev.RuleVersion = 1
	now := t0.Add(soak)
	res := Step(baseInput(testRule(nil), prev, withRaw(proposal(120), 120), now))
	n := res.Next
	if n.Active != nil || n.Shadow == nil || !n.ShadowSince.Equal(now) ||
		kinds(res.Events) != "cleared,shadow" {
		t.Fatalf("an old rule's soak was promoted by the new rule: %+v", res)
	}
}

func TestVersionChangeLeavesTheOperatorValue(t *testing.T) {
	prev := derivedState(180)
	prev.RuleVersion = 2
	in := baseInput(testRule(nil), prev, withRaw(proposal(180), 180), t0)
	in.OperatorSet, in.Operator, in.Running = true, 45, 45
	res := Step(in)
	if res.Next.Status != StatusOperator || res.Next.Value != 45 || res.Apply ||
		res.Next.Active != nil || !strings.HasPrefix(kinds(res.Events), "cleared") {
		t.Fatalf("result %+v", res)
	}
}
