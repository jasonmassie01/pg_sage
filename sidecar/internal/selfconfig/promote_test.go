package selfconfig

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// State transitions of one derived key (Step): shadow first, promotion
// only after the soak and only when the comparison is not worse, the
// operator's value and a pin always win, restart-bound keys change only at
// startup and are reported pending in between.

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const soak = 24 * time.Hour

func f(v float64) *float64 { return &v }

// testRule is a live rule whose comparison the test controls.
func testRule(verdict *Verdict) Rule {
	r := Rule{Key: "collector.interval_seconds", Name: "test_rule", Version: 3,
		Widens: WidensWhenLower, Cap: CapAtDefault, HardMin: 60, HardMax: 600, Step: 30,
		Get: func(c *config.Config) float64 { return float64(c.Collector.IntervalSeconds) },
		Set: func(c *config.Config, v float64) { c.Collector.IntervalSeconds = int(v) },
	}
	if verdict != nil {
		r.Compare = func(_, _ float64, _ []float64, _ *config.Config) Verdict { return *verdict }
	}
	return r
}

func proposal(v float64) Proposal {
	return Proposal{OK: true, Value: v, Reason: "derived for the test",
		Evidence: []Citation{{Name: "collector_cost_ms_per_cycle", Value: v * 10}},
		Bounds:   Bounds{Min: 60, Max: 600, Default: 60}}
}

func baseInput(r Rule, prev State, p Proposal, now time.Time) StepInput {
	return StepInput{Rule: r, Phase: PhaseLive, Now: now, Soak: soak, Prev: prev,
		Proposal: p, Running: 60, Default: 60, Cfg: config.DefaultConfig()}
}

func kinds(events []Event) string {
	var out []string
	for _, e := range events {
		out = append(out, string(e.Kind))
	}
	return strings.Join(out, ",")
}

func TestFirstCandidateGoesToShadowAndAppliesNothing(t *testing.T) {
	res := Step(baseInput(testRule(nil), State{}, proposal(120), t0))
	n := res.Next
	if n.Status != StatusShadow || n.Shadow == nil || *n.Shadow != 120 ||
		!n.ShadowSince.Equal(t0) || n.Active != nil || n.Value != 60 {
		t.Fatalf("next %+v", n)
	}
	if res.Apply {
		t.Fatal("a shadow candidate was applied")
	}
	if kinds(res.Events) != "shadow" || *res.Events[0].Value != 120 ||
		*res.Events[0].Previous != 60 || res.Events[0].Reason == "" {
		t.Fatalf("events %+v", res.Events)
	}
	if n.Key != "collector.interval_seconds" || n.Rule != "test_rule" || n.RuleVersion != 3 ||
		len(n.Evidence) != 1 || n.Bounds.Max != 600 {
		t.Fatalf("ledger identity %+v", n)
	}
}

func TestCandidateEqualToTheDefaultStaysDefault(t *testing.T) {
	res := Step(baseInput(testRule(nil), State{}, proposal(60), t0))
	if res.Next.Status != StatusDefault || res.Next.Shadow != nil || len(res.Events) != 0 ||
		res.Apply || res.Next.Value != 60 {
		t.Fatalf("result %+v", res)
	}
}

func shadowState(v float64, since time.Time) State {
	return State{Key: "collector.interval_seconds", Status: StatusShadow, Value: 60,
		Shadow: f(v), ShadowSince: since}
}

func TestPromotionWaitsForTheWholeSoak(t *testing.T) {
	notWorse := Verdict{Outcome: OutcomeNotWorse, Reason: "cost over budget"}
	r := testRule(&notWorse)
	prev := shadowState(120, t0)
	early := Step(baseInput(r, prev, proposal(120), t0.Add(soak-time.Nanosecond)))
	if early.Next.Status != StatusShadow || early.Next.Active != nil || early.Apply ||
		len(early.Events) != 0 {
		t.Fatalf("promoted before the soak ended: %+v", early)
	}
	onTime := Step(baseInput(r, prev, proposal(120), t0.Add(soak)))
	n := onTime.Next
	if n.Status != StatusDerived || n.Active == nil || *n.Active != 120 || n.Shadow != nil ||
		n.Value != 120 || !onTime.Apply {
		t.Fatalf("not promoted at the end of the soak: %+v", onTime)
	}
	if kinds(onTime.Events) != "promoted" || *onTime.Events[0].Previous != 60 ||
		!strings.Contains(onTime.Events[0].Reason, "cost over budget") {
		t.Fatalf("events %+v", onTime.Events)
	}
}

func TestWorseComparisonHoldsTheShadowWithItsReasonOnce(t *testing.T) {
	worse := Verdict{Outcome: OutcomeWorse, Reason: "the active value is within budget"}
	r := testRule(&worse)
	prev := shadowState(120, t0)
	first := Step(baseInput(r, prev, proposal(120), t0.Add(soak)))
	if first.Next.Status != StatusShadow || first.Next.Active != nil || first.Apply {
		t.Fatalf("promoted a worse candidate: %+v", first)
	}
	if kinds(first.Events) != "held" || first.Events[0].Reason != worse.Reason ||
		first.Next.ShadowReason != worse.Reason {
		t.Fatalf("events %+v next %+v", first.Events, first.Next)
	}
	again := Step(baseInput(r, first.Next, proposal(120), t0.Add(soak+time.Hour)))
	if len(again.Events) != 0 {
		t.Fatalf("the same hold reason was recorded twice: %+v", again.Events)
	}
	worse.Reason = "now a different reason"
	changed := Step(baseInput(r, again.Next, proposal(120), t0.Add(soak+2*time.Hour)))
	if kinds(changed.Events) != "held" || changed.Next.ShadowReason != worse.Reason {
		t.Fatalf("a new hold reason was not recorded: %+v", changed.Events)
	}
}

func TestInsufficientSamplesHold(t *testing.T) {
	r := rule(t, "collector.interval_seconds")
	prev := shadowState(120, t0)
	in := baseInput(r, prev, proposal(120), t0.Add(soak))
	in.Sample = f(2000)
	res := Step(in)
	if res.Next.Active != nil || kinds(res.Events) != "held" ||
		!strings.Contains(res.Events[0].Reason, "1 of 3") {
		t.Fatalf("insufficient soak evidence promoted or unexplained: %+v", res)
	}
	if len(res.Next.Samples) != 1 || res.Next.Samples[0] != 2000 {
		t.Fatalf("sample not kept: %v", res.Next.Samples)
	}
}

func TestSamplesAreCapped(t *testing.T) {
	prev := shadowState(120, t0)
	for i := 0; i < MaxSoakSamples; i++ {
		prev.Samples = append(prev.Samples, float64(i))
	}
	in := baseInput(testRule(nil), prev, proposal(120), t0.Add(time.Hour))
	in.Sample = f(-7)
	res := Step(in)
	s := res.Next.Samples
	if len(s) != MaxSoakSamples || s[len(s)-1] != -7 || s[0] != 1 {
		t.Fatalf("samples len %d first %v last %v", len(s), s[0], s[len(s)-1])
	}
}

func TestChangedCandidateRestartsTheSoak(t *testing.T) {
	prev := shadowState(120, t0)
	prev.Samples = []float64{1, 2, 3}
	res := Step(baseInput(testRule(nil), prev, proposal(150), t0.Add(soak+time.Hour)))
	n := res.Next
	if n.Shadow == nil || *n.Shadow != 150 || !n.ShadowSince.Equal(t0.Add(soak+time.Hour)) ||
		len(n.Samples) != 0 || n.Active != nil {
		t.Fatalf("soak not restarted: %+v", n)
	}
	if kinds(res.Events) != "shadow" || *res.Events[0].Value != 150 {
		t.Fatalf("events %+v", res.Events)
	}
}

func TestCandidateBackToTheActiveValueClearsTheShadow(t *testing.T) {
	prev := shadowState(120, t0)
	prev.Active, prev.Value = f(90), 90
	res := Step(baseInput(testRule(nil), prev, proposal(90), t0.Add(time.Hour)))
	if res.Next.Shadow != nil || res.Next.Status != StatusDerived || res.Next.Value != 90 ||
		kinds(res.Events) != "cleared" {
		t.Fatalf("result %+v", res)
	}
}

func TestStabilityPromotionWithoutAMeasurableOutcome(t *testing.T) {
	r := testRule(nil) // no comparison: the soak itself is the test
	res := Step(baseInput(r, shadowState(120, t0), proposal(120), t0.Add(soak)))
	if res.Next.Active == nil || *res.Next.Active != 120 || kinds(res.Events) != "promoted" ||
		!strings.Contains(res.Events[0].Reason, "no measurable outcome") {
		t.Fatalf("result %+v", res)
	}
}

func TestMissingEvidenceKeepsTheActiveValue(t *testing.T) {
	prev := State{Key: "collector.interval_seconds", Status: StatusDerived, Value: 120,
		Active: f(120)}
	in := baseInput(testRule(nil), prev, Proposal{Reason: "self-cost not measured"}, t0)
	in.Running = 120
	res := Step(in)
	if res.Next.Status != StatusDerived || res.Next.Value != 120 || res.Apply ||
		len(res.Events) != 0 || res.Next.Note != "self-cost not measured" {
		t.Fatalf("result %+v", res)
	}
}

func TestOperatorValueAlwaysWins(t *testing.T) {
	prev := shadowState(120, t0)
	prev.Active, prev.Value, prev.Status = f(90), 90, StatusShadow
	in := baseInput(testRule(nil), prev, proposal(150), t0.Add(soak))
	in.OperatorSet, in.Operator, in.Running = true, 45, 90
	res := Step(in)
	n := res.Next
	if n.Status != StatusOperator || n.Operator == nil || *n.Operator != 45 ||
		n.Shadow != nil || res.Apply || n.Value != 90 {
		t.Fatalf("operator did not win: %+v", res)
	}
	if n.Pending == nil || *n.Pending != 45 {
		t.Fatalf("operator value not reported pending while the runtime runs 90: %+v", n)
	}
	if kinds(res.Events) != "operator_set" || *res.Events[0].Value != 45 {
		t.Fatalf("events %+v", res.Events)
	}
	// Still set on the next pass: no repeated event.
	in.Prev, in.Running = n, 45
	res = Step(in)
	if len(res.Events) != 0 || res.Next.Pending != nil || res.Next.Value != 45 {
		t.Fatalf("second pass %+v", res)
	}
	// Removed by the operator: derivation resumes from the active value.
	in.Prev, in.OperatorSet = res.Next, false
	res = Step(in)
	if !strings.HasPrefix(kinds(res.Events), "resumed") || res.Next.Operator != nil ||
		res.Next.Status == StatusOperator || res.Next.Value != 90 || !res.Apply {
		t.Fatalf("resume %+v", res)
	}
}

func TestPinnedValueWinsOverEvidence(t *testing.T) {
	prev := State{Key: "collector.interval_seconds", Status: StatusPinned, Value: 120,
		Active: f(120), Pinned: f(120), PinnedBy: "admin@x", PinnedAt: t0}
	in := baseInput(testRule(nil), prev, proposal(300), t0.Add(soak*3))
	in.Running = 120
	res := Step(in)
	if res.Next.Status != StatusPinned || res.Next.Value != 120 || res.Next.Shadow != nil ||
		res.Apply || len(res.Events) != 0 {
		t.Fatalf("pin did not hold: %+v", res)
	}
	// A reload reset the runtime to the default: the pin is re-applied.
	in.Running = 60
	if res := Step(in); !res.Apply || res.Next.Value != 120 {
		t.Fatalf("pin not re-applied after a reload: %+v", res)
	}
}

func restartRule() Rule {
	r, ok := Lookup("safety.query_timeout_ms")
	if !ok {
		panic("no rule for safety.query_timeout_ms")
	}
	r.Compare = nil
	return r
}

func TestRestartKeyPromotedLiveIsPendingUntilStartup(t *testing.T) {
	r := restartRule()
	prev := State{Key: r.Key, Status: StatusShadow, Value: 500, Shadow: f(1500),
		ShadowSince: t0}
	in := StepInput{Rule: r, Restart: true, Phase: PhaseLive, Now: t0.Add(soak), Soak: soak,
		Prev: prev, Proposal: Proposal{OK: true, Value: 1500}, Running: 500, Default: 500,
		Cfg: config.DefaultConfig()}
	res := Step(in)
	n := res.Next
	if res.Apply || n.Value != 500 || n.Pending == nil || *n.Pending != 1500 ||
		n.Active == nil || *n.Active != 1500 || n.Status != StatusDerived {
		t.Fatalf("live promotion of a restart key: %+v", res)
	}
	if kinds(res.Events) != "promoted" || !strings.Contains(res.Events[0].Reason, "restart") {
		t.Fatalf("events %+v", res.Events)
	}
	// Next start: the pending value takes effect, recorded once.
	in.Phase, in.Prev, in.Now, in.Running = PhaseStartup, n, t0.Add(soak+time.Hour), 500
	start := Step(in)
	if !start.Apply || start.Next.Value != 1500 || start.Next.Pending != nil ||
		kinds(start.Events) != "applied" || *start.Events[0].Previous != 500 {
		t.Fatalf("startup %+v", start)
	}
	// A later start with the value already active records nothing new.
	in.Prev, in.Running = start.Next, 500
	again := Step(in)
	if !again.Apply || len(again.Events) != 0 {
		t.Fatalf("second startup %+v", again)
	}
}

// A restart-bound key keeps the value it started with: a reload that reset
// the runtime config is undone (the startup value restored), but a new
// value never takes effect before the next start.
func TestRestartKeyIsRestoredButNeverChangedLive(t *testing.T) {
	r := restartRule()
	prev := State{Key: r.Key, Status: StatusDerived, Value: 900, Active: f(1200),
		Pending: f(1200)}
	in := StepInput{Rule: r, Restart: true, Phase: PhaseLive, Now: t0, Soak: soak, Prev: prev,
		Proposal: Proposal{OK: true, Value: 1200}, Running: 500, Default: 500,
		Cfg: config.DefaultConfig()}
	res := Step(in)
	if !res.Apply || res.Next.Value != 900 || res.Next.Pending == nil ||
		*res.Next.Pending != 1200 || len(res.Events) != 0 {
		t.Fatalf("restart key not restored to its startup value or changed live: %+v", res)
	}
	in.Prev, in.Running = res.Next, 900
	res = Step(in)
	if res.Apply || res.Next.Value != 900 || res.Next.Pending == nil || *res.Next.Pending != 1200 {
		t.Fatalf("steady state %+v", res)
	}
	// No startup state at all (first pass is live): the running value stays.
	in.Prev, in.Running = State{}, 500
	res = Step(in)
	if res.Apply || res.Next.Value != 500 {
		t.Fatalf("live pass without a startup state changed the runtime: %+v", res)
	}
}

func TestStartupPromotesASoakedRestartKeyDirectly(t *testing.T) {
	r := restartRule()
	prev := State{Key: r.Key, Status: StatusShadow, Value: 500, Shadow: f(800),
		ShadowSince: t0}
	in := StepInput{Rule: r, Restart: true, Phase: PhaseStartup, Now: t0.Add(soak), Soak: soak,
		Prev: prev, Proposal: Proposal{OK: true, Value: 800}, Running: 500, Default: 500,
		Cfg: config.DefaultConfig()}
	res := Step(in)
	if !res.Apply || res.Next.Value != 800 || res.Next.Pending != nil ||
		kinds(res.Events) != "promoted" {
		t.Fatalf("result %+v", res)
	}
}

func TestEqualTolerance(t *testing.T) {
	if !equal(60, 60+1e-12) || equal(60, 60.001) {
		t.Fatal("equal tolerance wrong")
	}
}
