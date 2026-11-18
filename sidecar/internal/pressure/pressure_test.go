package pressure

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/policy"
)

// guard-01a: the verdict arithmetic. Evaluate is a pure function of its
// readings and limits, so these tests drive every band, boundary and
// unknown rule directly. No concurrent access tests here: Evaluate keeps
// no state (the checker's tests cover concurrency).

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func f(v float64) *float64 { return &v }

// idleReadings is a primary with one caught-up replica and nothing on
// the target table.
func idleReadings() map[string]Reading {
	return map[string]Reading{
		policy.PressureReplicaLag:   Known(0, t0),
		policy.PressureXIDAge:       Known(1_000_000, t0),
		policy.PressureMultixactAge: Known(10, t0),
		policy.PressureVacuum:       Known(0, t0),
		policy.PressureWALRate:      Known(1024, t0),
		policy.PressureLockQueue:    Known(0, t0),
		policy.PressureFreeStorage:  Known(80, t0),
	}
}

func evalWith(readings map[string]Reading, mutate func(*Evaluation)) Verdict {
	e := Evaluation{Readings: readings, Limits: DefaultLimits(Alerts{}),
		XIDFreezeMax: 200_000_000, MXIDFreezeMax: 400_000_000, Batch: 1000,
		MinSleep: 2 * time.Second, MaxSleep: time.Minute, Now: t0}
	if mutate != nil {
		mutate(&e)
	}
	return Evaluate(e)
}

func inputOf(t *testing.T, v Verdict, name string) Input {
	t.Helper()
	for _, in := range v.Inputs {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("verdict has no input %s: %+v", name, v.Inputs)
	return Input{}
}

func TestPressureEvaluate_IdleProceedsWithEveryInputReported(t *testing.T) {
	v := evalWith(idleReadings(), nil)
	if v.Verdict != Proceed || v.BatchFactor != 1 || v.SuggestedBatch != 1000 ||
		v.SleepSeconds != 0 {
		t.Fatalf("idle verdict = %s factor %v batch %d sleep %v, want proceed 1 1000 0",
			v.Verdict, v.BatchFactor, v.SuggestedBatch, v.SleepSeconds)
	}
	if len(v.Inputs) != len(policy.PressureInputs()) {
		t.Fatalf("inputs = %d, want all %d", len(v.Inputs), len(policy.PressureInputs()))
	}
	for i, in := range v.Inputs {
		if in.Name != policy.PressureInputs()[i] {
			t.Fatalf("input %d = %s, want the fixed order %v", i, in.Name,
				policy.PressureInputs())
		}
		if in.State != StateKnown || in.Band != BandOK || in.Value == nil {
			t.Errorf("%s = %+v, want known and ok with a value", in.Name, in)
		}
		if in.ObservedAt == nil || !in.ObservedAt.Equal(t0) || in.AgeSeconds == nil {
			t.Errorf("%s freshness = %v/%v, want observed at t0", in.Name, in.ObservedAt,
				in.AgeSeconds)
		}
	}
	lag := inputOf(t, v, policy.PressureReplicaLag)
	if lag.StopLimit == nil || *lag.StopLimit != 15 || lag.SlowLimit == nil ||
		*lag.SlowLimit != 7.5 || !lag.Required || lag.Unit != "s" {
		t.Fatalf("lag limits = %+v, want slow 7.5 stop 15 required, in s", lag)
	}
}

func TestPressureEvaluate_LagBoundaries(t *testing.T) {
	cases := []struct {
		lag  float64
		want Kind
	}{
		{15, Stop}, {14.999, Slow}, {7.5, Slow}, {7.499, Proceed}, {120, Stop},
	}
	for _, c := range cases {
		r := idleReadings()
		r[policy.PressureReplicaLag] = Known(c.lag, t0)
		if v := evalWith(r, nil); v.Verdict != c.want {
			t.Errorf("lag %v s = %s, want %s", c.lag, v.Verdict, c.want)
		}
	}
}

func TestPressureEvaluate_SlowHalvesTheBatchPerSlowInput(t *testing.T) {
	r := idleReadings()
	r[policy.PressureReplicaLag] = Known(8, t0)
	r[policy.PressureLockQueue] = Known(1, t0)
	v := evalWith(r, nil)
	if v.Verdict != Slow || v.BatchFactor != 0.25 || v.SuggestedBatch != 250 {
		t.Fatalf("two slow inputs = %s factor %v batch %d, want slow 0.25 250", v.Verdict,
			v.BatchFactor, v.SuggestedBatch)
	}
	if len(v.Reasons) != 2 {
		t.Fatalf("reasons = %v, want one per slow input", v.Reasons)
	}
	small := evalWith(r, func(e *Evaluation) { e.Batch = 1 })
	if small.SuggestedBatch != 1 {
		t.Fatalf("batch 1 halved twice = %d, want at least 1", small.SuggestedBatch)
	}
	none := evalWith(r, func(e *Evaluation) { e.Batch = 0 })
	if none.SuggestedBatch != 0 || none.BatchFactor != 0.25 {
		t.Fatalf("no batch given = %d factor %v, want no size and the factor",
			none.SuggestedBatch, none.BatchFactor)
	}
}

func TestPressureEvaluate_StopZeroesTheBatch(t *testing.T) {
	r := idleReadings()
	r[policy.PressureReplicaLag] = Known(20, t0)
	r[policy.PressureLockQueue] = Known(1, t0)
	v := evalWith(r, nil)
	if v.Verdict != Stop || v.BatchFactor != 0 || v.SuggestedBatch != 0 {
		t.Fatalf("stop = %s factor %v batch %d, want stop 0 0", v.Verdict, v.BatchFactor,
			v.SuggestedBatch)
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), policy.PressureReplicaLag) {
		t.Fatalf("reasons %v do not name the stopping input", v.Reasons)
	}
}

func TestPressureEvaluate_SleepFollowsTheCatchUpRate(t *testing.T) {
	r := idleReadings()
	r[policy.PressureReplicaLag] = Known(10, t0)
	cases := []struct {
		name   string
		rate   float64
		known  bool
		expect float64
	}{
		{"measured", 0.5, true, 5},             // (10-7.5)/0.5
		{"fast catch-up floors", 100, true, 2}, // 0.025 s, at least the cache period
		{"unmeasured uses the excess", 0, false, 2.5},
		{"falling behind waits the most", -0.2, true, 60},
		{"slow catch-up caps", 0.001, true, 60},
	}
	for _, c := range cases {
		v := evalWith(r, func(e *Evaluation) { e.CatchUp, e.CatchUpKnown = c.rate, c.known })
		if math.Abs(v.SleepSeconds-c.expect) > 1e-9 {
			t.Errorf("%s: sleep = %v, want %v", c.name, v.SleepSeconds, c.expect)
		}
	}
	other := idleReadings()
	other[policy.PressureLockQueue] = Known(1, t0)
	if v := evalWith(other, nil); v.SleepSeconds != 2 {
		t.Fatalf("slow without lag sleeps %v, want the cache period 2", v.SleepSeconds)
	}
}

func TestPressureEvaluate_UnknownRequiredInputStops(t *testing.T) {
	r := idleReadings()
	r[policy.PressureLockQueue] = Unknown("timed out after 1s")
	v := evalWith(r, nil)
	if v.Verdict != Stop {
		t.Fatalf("unknown required lock queue = %s, want stop", v.Verdict)
	}
	in := inputOf(t, v, policy.PressureLockQueue)
	if in.State != StateUnknown || in.Value != nil || in.Reason != "timed out after 1s" {
		t.Fatalf("unknown input = %+v, want unknown with its reason and no value", in)
	}
	if !strings.Contains(strings.Join(v.Reasons, ";"), "lock_queue is unknown") {
		t.Fatalf("reasons %v do not say the required input is unknown", v.Reasons)
	}
}

func TestPressureEvaluate_UnknownOptionalInputIsReportedNotDecisive(t *testing.T) {
	r := idleReadings()
	r[policy.PressureFreeStorage] = Unknown("no cloud storage adapter")
	v := evalWith(r, nil)
	if v.Verdict != Proceed {
		t.Fatalf("unknown optional storage = %s, want the worst known (proceed)", v.Verdict)
	}
	if in := inputOf(t, v, policy.PressureFreeStorage); in.State != StateUnknown ||
		in.Required || in.Reason == "" {
		t.Fatalf("storage = %+v, want unknown, optional, with a reason", in)
	}
	r[policy.PressureReplicaLag] = Known(9, t0)
	if v := evalWith(r, nil); v.Verdict != Slow {
		t.Fatalf("slow lag with unknown storage = %s, want slow", v.Verdict)
	}
}

func TestPressureEvaluate_NothingKnownNeverProceeds(t *testing.T) {
	r := map[string]Reading{}
	for _, name := range policy.PressureInputs() {
		r[name] = Unknown("connection refused")
	}
	v := evalWith(r, func(e *Evaluation) { e.Limits.Required = map[string]bool{} })
	if v.Verdict != Stop {
		t.Fatalf("every input unknown, none required = %s, want stop", v.Verdict)
	}
	empty := evalWith(nil, func(e *Evaluation) { e.Limits.Required = map[string]bool{} })
	if empty.Verdict != Stop || len(empty.Inputs) != len(policy.PressureInputs()) {
		t.Fatalf("no readings = %s with %d inputs, want stop reporting every input",
			empty.Verdict, len(empty.Inputs))
	}
	for _, in := range empty.Inputs {
		if in.State != StateUnknown || in.Reason == "" {
			t.Errorf("missing reading %s = %+v, want unknown with a reason", in.Name, in)
		}
	}
}

func TestPressureEvaluate_NoReplicasIsNotApplicable(t *testing.T) {
	r := idleReadings()
	r[policy.PressureReplicaLag] = NotApplicable("no replicas")
	v := evalWith(r, nil)
	if v.Verdict != Proceed {
		t.Fatalf("no replicas = %s, want proceed (not applicable is not unknown)", v.Verdict)
	}
	lag := inputOf(t, v, policy.PressureReplicaLag)
	if lag.State != StateNotApplicable || lag.Band != "" || lag.Reason != "no replicas" {
		t.Fatalf("lag = %+v, want not_applicable with its reason and no band", lag)
	}
}

func TestPressureEvaluate_NonFiniteValueIsUnknown(t *testing.T) {
	r := idleReadings()
	r[policy.PressureWALRate] = Known(math.NaN(), t0)
	r[policy.PressureXIDAge] = Known(math.Inf(1), t0)
	v := evalWith(r, nil)
	if v.Verdict != Stop {
		t.Fatalf("an infinite required xid age = %s, want stop", v.Verdict)
	}
	if in := inputOf(t, v, policy.PressureWALRate); in.State != StateUnknown ||
		in.Value != nil {
		t.Fatalf("NaN WAL rate = %+v, want unknown with no value", in)
	}
}

func TestPressureEvaluate_FreeStorageIsLowerWorse(t *testing.T) {
	for _, c := range []struct {
		pct  float64
		want Kind
	}{{50, Proceed}, {30, Slow}, {25, Slow}, {20, Stop}, {3, Stop}} {
		r := idleReadings()
		r[policy.PressureFreeStorage] = Known(c.pct, t0)
		if v := evalWith(r, nil); v.Verdict != c.want {
			t.Errorf("free storage %v%% = %s, want %s", c.pct, v.Verdict, c.want)
		}
	}
}

func TestPressureEvaluate_XIDSlowsFromTheFreezeMaximum(t *testing.T) {
	cases := []struct {
		age, freezeMax float64
		want           Kind
	}{
		{199_999_999, 200_000_000, Proceed},
		{200_000_000, 200_000_000, Slow},
		{250_000_000, 200_000_000, Stop},
		{124_999_999, 300_000_000, Proceed}, // freeze max past stop: slow at stop/2
		{125_000_000, 300_000_000, Slow},
		{125_000_000, 0, Slow}, // freeze max unread: stop/2
	}
	for _, c := range cases {
		r := idleReadings()
		r[policy.PressureXIDAge] = Known(c.age, t0)
		v := evalWith(r, func(e *Evaluation) { e.XIDFreezeMax = c.freezeMax })
		if v.Verdict != c.want {
			t.Errorf("xid age %v with freeze max %v = %s, want %s", c.age, c.freezeMax,
				v.Verdict, c.want)
		}
	}
	r := idleReadings()
	r[policy.PressureMultixactAge] = Known(130_000_000, t0)
	if v := evalWith(r, nil); v.Verdict != Slow {
		t.Fatalf("multixact 130M with freeze max 400M = %s, want slow (stop/2)", v.Verdict)
	}
}

func TestPressureEvaluate_VacuumOnTheTargetSlowsButNeverStops(t *testing.T) {
	r := idleReadings()
	r[policy.PressureVacuum] = Known(3, t0)
	v := evalWith(r, nil)
	if v.Verdict != Slow {
		t.Fatalf("vacuum running = %s, want slow", v.Verdict)
	}
	if in := inputOf(t, v, policy.PressureVacuum); in.StopLimit != nil || in.Band != BandSlow {
		t.Fatalf("vacuum input = %+v, want no stop limit and the slow band", in)
	}
}

func TestPressureDefaultLimits_AreHalfTheAlertThresholds(t *testing.T) {
	d := DefaultLimits(Alerts{})
	check := func(name string, slow, stop *float64) {
		t.Helper()
		got := d.Inputs[name]
		if !sameBound(got.Slow, slow) || !sameBound(got.Stop, stop) {
			t.Errorf("%s = slow %v stop %v, want slow %v stop %v", name, deref(got.Slow),
				deref(got.Stop), deref(slow), deref(stop))
		}
	}
	check(policy.PressureReplicaLag, f(7.5), f(15))
	check(policy.PressureXIDAge, nil, f(250_000_000))
	check(policy.PressureMultixactAge, nil, f(250_000_000))
	check(policy.PressureVacuum, f(1), nil)
	check(policy.PressureLockQueue, f(1), f(3))
	check(policy.PressureFreeStorage, f(30), f(20))
	check(policy.PressureWALRate, f(32<<20), f(64<<20))
	if !d.Inputs[policy.PressureXIDAge].SlowAtFreezeMax {
		t.Errorf("xid default slow is not the freeze maximum")
	}
	for _, name := range []string{policy.PressureReplicaLag, policy.PressureXIDAge,
		policy.PressureMultixactAge, policy.PressureVacuum, policy.PressureLockQueue} {
		if !d.Required[name] {
			t.Errorf("%s is not required by default", name)
		}
	}
	if d.Required[policy.PressureWALRate] || d.Required[policy.PressureFreeStorage] {
		t.Errorf("WAL rate or free storage required by default: %v", d.Required)
	}
	tuned := DefaultLimits(Alerts{ReplicaLagSeconds: 60, XIDWarning: 1_000_000_000,
		MinFreeStoragePct: 25})
	if *tuned.Inputs[policy.PressureReplicaLag].Stop != 30 ||
		*tuned.Inputs[policy.PressureXIDAge].Stop != 500_000_000 ||
		*tuned.Inputs[policy.PressureFreeStorage].Stop != 50 ||
		*tuned.Inputs[policy.PressureFreeStorage].Slow != 75 {
		t.Fatalf("defaults do not follow the alert thresholds: %+v", tuned.Inputs)
	}
	capped := DefaultLimits(Alerts{MinFreeStoragePct: 60})
	if *capped.Inputs[policy.PressureFreeStorage].Stop != 100 ||
		*capped.Inputs[policy.PressureFreeStorage].Slow != 100 {
		t.Fatalf("storage defaults past 100%% = %+v, want capped at 100",
			capped.Inputs[policy.PressureFreeStorage])
	}
}

func TestPressureLimitsFromPolicy_OverridesOnlyWhatIsSet(t *testing.T) {
	p := policy.PressurePolicy{
		Limits: map[string]policy.PressureLimit{
			policy.PressureReplicaLag: {Stop: f(10)},
			policy.PressureXIDAge:     {Slow: f(100)},
		},
		Required: []string{policy.PressureFreeStorage},
	}
	l := LimitsFromPolicy(p, Alerts{})
	lag := l.Inputs[policy.PressureReplicaLag]
	if *lag.Stop != 10 || *lag.Slow != 7.5 {
		t.Fatalf("lag = %v/%v, want the set stop 10 and the default slow 7.5",
			deref(lag.Slow), deref(lag.Stop))
	}
	if xid := l.Inputs[policy.PressureXIDAge]; xid.SlowAtFreezeMax || *xid.Slow != 100 {
		t.Fatalf("xid = %+v, want the set slow 100 instead of the freeze maximum", xid)
	}
	if len(l.Required) != 1 || !l.Required[policy.PressureFreeStorage] {
		t.Fatalf("required = %v, want only free storage", l.Required)
	}
	none := LimitsFromPolicy(policy.PressurePolicy{Required: []string{}}, Alerts{})
	if len(none.Required) != 0 {
		t.Fatalf("an empty required list = %v, want nothing required", none.Required)
	}
	zero := LimitsFromPolicy(policy.PressurePolicy{}, Alerts{})
	if len(zero.Required) != 5 {
		t.Fatalf("no block = %v, want the default required inputs", zero.Required)
	}
}

// A stop set below the default slow (or, for free storage, above it)
// keeps a slow band: half the stop, or for free storage half again above
// it, at most 100%. A slow the policy sets itself is never moved.
func TestPressureLimitsFromPolicy_StopAloneKeepsASlowBand(t *testing.T) {
	p := policy.PressurePolicy{Limits: map[string]policy.PressureLimit{
		policy.PressureReplicaLag:  {Stop: f(4)},
		policy.PressureLockQueue:   {Stop: f(1)},
		policy.PressureFreeStorage: {Stop: f(40)},
		policy.PressureWALRate:     {Stop: f(1 << 20), Slow: f(1 << 10)},
	}}
	l := LimitsFromPolicy(p, Alerts{})
	for name, want := range map[string]float64{policy.PressureReplicaLag: 2,
		policy.PressureLockQueue: 0.5, policy.PressureFreeStorage: 60,
		policy.PressureWALRate: 1 << 10} {
		if got := l.Inputs[name].Slow; got == nil || *got != want {
			t.Errorf("%s slow = %v, want %v under its stop", name, deref(got), want)
		}
	}
	v := Evaluate(Evaluation{Readings: map[string]Reading{
		policy.PressureReplicaLag: Known(3, time.Now())}, Limits: l, Now: time.Now()})
	if in := v.Inputs[0]; in.Band != BandSlow {
		t.Fatalf("lag 3 s under a set stop of 4 = %s, want the slow band", in.Band)
	}
	high := LimitsFromPolicy(policy.PressurePolicy{Limits: map[string]policy.PressureLimit{
		policy.PressureFreeStorage: {Stop: f(90)}}}, Alerts{})
	if got := *high.Inputs[policy.PressureFreeStorage].Slow; got != 100 {
		t.Fatalf("free storage slow over a stop of 90 = %v, want capped at 100", got)
	}
}

func TestPressureInputFailure_NamesTheCause(t *testing.T) {
	timeout := &pgconn.PgError{Code: "57014", Message: "canceling statement due to " +
		"statement timeout"}
	cases := []struct {
		err  error
		want string
	}{
		{timeout, "timed out after 1s"},
		{context.DeadlineExceeded, "timed out after 1s"},
		{&pgconn.PgError{Code: "42501", Message: "permission denied for view x"},
			"permission denied"},
		{errors.New("conn closed"), "conn closed"},
	}
	for _, c := range cases {
		if got := inputFailure(c.err, time.Second); !strings.Contains(got, c.want) {
			t.Errorf("inputFailure(%v) = %q, want it to contain %q", c.err, got, c.want)
		}
	}
}

func sameBound(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
