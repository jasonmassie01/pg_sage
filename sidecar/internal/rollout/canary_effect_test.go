package rollout

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-03b: a fleet canary judges each treated database by a number. Its
// effect is its own v2 verdict's effect, or, with at least two untreated
// members measured over the same hours, its targets' change minus the
// median change of the same queries (matched by normalized text) on the
// untreated members. With no v2 number the verdict flag decides, and the
// detail says "flag only".

// effectQuery is the normalized text of the change's target query.
const effectQuery = "select * from orders where status = $1"

// effectTarget is a fakeTarget that reads its v2 effect and its query
// history (EffectReader) and records tier-4 ladder records.
type effectTarget struct {
	*fakeTarget
	effect    MemberEffect
	effectErr error
	change    float64
	ran       bool
	changeErr error
	// gate runs inside QueryChange (the parallel-read test's barrier).
	gate    func() error
	ladder  []verify.LadderRecord
	changes int
	order   *callOrder
}

func newEffectTarget(name string, v2Pct, changePct float64) *effectTarget {
	ft := newTarget(name, canarySQL)
	ft.settleAfter = 0
	return &effectTarget{fakeTarget: ft, change: changePct, ran: true,
		effect: measuredEffect(v2Pct)}
}

// measuredEffect is a measured v2 record over yesterday's hours.
func measuredEffect(pct float64) MemberEffect {
	to := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	return MemberEffect{Measured: true, EffectPct: pct, CILowPct: pct - 2,
		CIHighPct: pct + 2, Queries: []string{effectQuery}, From: to.Add(-24 * time.Hour),
		To: to}
}

func (e *effectTarget) ActionEffect(context.Context, int64) (MemberEffect, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.effect, e.effectErr
}

func (e *effectTarget) QueryChange(_ context.Context, queries []string, from,
	to time.Time) (float64, bool, error) {
	e.mu.Lock()
	gate := e.gate
	e.changes++
	e.mu.Unlock()
	if len(queries) != 1 || queries[0] != effectQuery || !from.Before(to) {
		return 0, false, fmt.Errorf("unexpected query change request %q %v..%v", queries,
			from, to)
	}
	if gate != nil {
		if err := gate(); err != nil {
			return 0, false, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.change, e.ran, e.changeErr
}

func (e *effectTarget) RecordFleetLadder(_ context.Context, _ int64,
	rec verify.LadderRecord) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ladder = append(e.ladder, rec)
	return nil
}

func (e *effectTarget) Rollback(ctx context.Context, id int64, reason string) error {
	if e.order != nil {
		e.order.add(e.name)
	}
	return e.fakeTarget.Rollback(ctx, id, reason)
}

// callOrder records the order of rollbacks across databases.
type callOrder struct {
	mu    sync.Mutex
	names []string
}

func (c *callOrder) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
}

func (c *callOrder) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.names, ",")
}

// logLines collects the service's log lines.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// measureMember measures self's action 101 (settled as success) with
// order as the canary's targets and the ones in treated already changed.
func measureMember(t *testing.T, members map[string]CanaryTarget, order []string,
	self string, treated ...string) (Outcome, *logLines) {
	t.Helper()
	logs := &logLines{}
	svc, err := NewCanaryService(func(name string) (CanaryTarget, error) {
		target, ok := members[name]
		if !ok {
			return nil, fmt.Errorf("database %q is not in the fleet", name)
		}
		return target, nil
	}, &PostgresRunStore{}, CanaryOptions{RegressionLimitPct: 10, Log: logs.logf,
		PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	settle(t, members[self], 101, "success", "success")
	b := &canaryBackend{svc: svc, findings: map[string]int{}, actions: map[string]int64{},
		targets: order}
	b.actions[self] = 101
	for _, name := range treated {
		b.actions[name] = 101
	}
	o, err := b.Measure(context.Background(), Instance{ID: self},
		AppliedChange{Handle: "101"})
	if err != nil {
		t.Fatalf("measure %s: %v", self, err)
	}
	return o, logs
}

// settle gives target an action with a final verdict.
func settle(t *testing.T, target CanaryTarget, id int64, outcome, verification string) {
	t.Helper()
	var ft *fakeTarget
	switch x := target.(type) {
	case *effectTarget:
		ft = x.fakeTarget
	case *fakeTarget:
		ft = x
	default:
		t.Fatalf("cannot settle a %T", target)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.results[id] = [2]string{outcome, verification}
}

func members(targets ...*effectTarget) map[string]CanaryTarget {
	out := map[string]CanaryTarget{}
	for _, target := range targets {
		out[target.name] = target
	}
	return out
}

func near(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 1e-9
}

func fmtPtr(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%v", *p)
}

// Happy path: a improves 30% while the untreated b and c are flat: its
// effect is -30 with a fleet control of 0, and RegressionPct is that number.
func TestEffectSubtractsTheFleetControlOfUntreatedMembers(t *testing.T) {
	a, b, c := newEffectTarget("a", -28, -30), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 0, 0)
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.Failed || o.EffectMethod != EffectFleetControl || !near(o.EffectPct, -30) ||
		!near(o.ControlPct, 0) || o.RegressionPct != -30 {
		t.Fatalf("outcome = %+v (effect %s control %s)", o, fmtPtr(o.EffectPct),
			fmtPtr(o.ControlPct))
	}
	if !near(o.CILowPct, -30) || !near(o.CIHighPct, -26) {
		t.Fatalf("CI = %s..%s, want the member's v2 CI -30..-26", fmtPtr(o.CILowPct),
			fmtPtr(o.CIHighPct))
	}
	if !strings.Contains(o.Detail, "fleet control") || !strings.Contains(o.Detail, "b, c") {
		t.Fatalf("detail %q does not name the fleet control and its members", o.Detail)
	}
	if b.changes != 1 || c.changes != 1 || a.changes != 1 {
		t.Fatalf("query change reads a=%d b=%d c=%d, want one each", a.changes, b.changes,
			c.changes)
	}
}

// A fleet-wide shift: every member, treated or not, slows 20% the same
// hours. The effect is about 0, so the shift alone never halts.
func TestEffectFleetWideShiftCancelsOut(t *testing.T) {
	a, b, c := newEffectTarget("a", 20, 20), newEffectTarget("b", 0, 20),
		newEffectTarget("c", 0, 21)
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.EffectMethod != EffectFleetControl || !near(o.ControlPct, 20.5) ||
		!near(o.EffectPct, -0.5) || o.RegressionPct != -0.5 {
		t.Fatalf("outcome = %+v (effect %s control %s)", o, fmtPtr(o.EffectPct),
			fmtPtr(o.ControlPct))
	}
}

// The fleet control is the median, not the mean: one noisy member does
// not move it.
func TestEffectFleetControlIsTheMedian(t *testing.T) {
	a := newEffectTarget("a", 0, 5)
	b, c, d := newEffectTarget("b", 0, 1), newEffectTarget("c", 0, 2),
		newEffectTarget("d", 0, 90)
	o, _ := measureMember(t, members(a, b, c, d), []string{"a", "b", "c", "d"}, "a")
	if !near(o.ControlPct, 2) || !near(o.EffectPct, 3) {
		t.Fatalf("control %s effect %s, want the median 2 and 5-2", fmtPtr(o.ControlPct),
			fmtPtr(o.EffectPct))
	}
}

// Members already treated are never controls.
func TestEffectTreatedMembersAreNotControls(t *testing.T) {
	a, b, c, d := newEffectTarget("a", 0, 0), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 4, 4), newEffectTarget("d", 0, 0)
	o, _ := measureMember(t, members(a, b, c, d), []string{"a", "b", "c", "d"}, "c", "a",
		"b")
	if a.changes != 0 || b.changes != 0 || o.EffectMethod != EffectOwnV2 ||
		!near(o.EffectPct, 4) || o.RegressionPct != 4 {
		t.Fatalf("treated read as controls (a=%d b=%d) or outcome %+v", a.changes,
			b.changes, o)
	}
	if !strings.Contains(o.Detail, "no fleet control") {
		t.Fatalf("detail %q does not say there is no fleet control", o.Detail)
	}
}

// Invalid input: a v2 record that is not an object is "flag only" with
// a WARN naming the database; the flag (a kept success) passes it.
func TestEffectMalformedV2RecordIsFlagOnlyWithAWarning(t *testing.T) {
	a := newEffectTarget("a", 0, 0)
	a.effect = MemberEffect{Malformed: true,
		Reason: "the v2 record is a string, not a JSON object"}
	o, logs := measureMember(t, members(a), []string{"a"}, "a")
	if o.Failed || o.EffectMethod != EffectFlagOnly || o.EffectPct != nil ||
		o.RegressionPct != 0 || !strings.Contains(o.Detail, "flag only") {
		t.Fatalf("outcome = %+v", o)
	}
	if got := logs.joined(); !strings.Contains(got, "a") ||
		!strings.Contains(got, "not a JSON object") {
		t.Fatalf("no warning names the malformed record: %q", got)
	}
}

// Nil/empty: a one-member fleet has no fleet control; the member's own
// v2 effect decides, and the detail says why there is no control.
func TestEffectOneMemberFleetUsesItsOwnV2Effect(t *testing.T) {
	a := newEffectTarget("a", 12.5, -40)
	o, _ := measureMember(t, members(a), []string{"a"}, "a")
	if o.EffectMethod != EffectOwnV2 || !near(o.EffectPct, 12.5) || o.ControlPct != nil ||
		o.RegressionPct != 12.5 || !strings.Contains(o.Detail, "no fleet control") {
		t.Fatalf("outcome = %+v", o)
	}
}

// Nil/empty: a change with no target query text has no fleet control.
func TestEffectWithoutTargetQueriesHasNoFleetControl(t *testing.T) {
	a, b, c := newEffectTarget("a", 3, 3), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 0, 0)
	a.effect.Queries = nil
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.EffectMethod != EffectOwnV2 || !near(o.EffectPct, 3) || b.changes != 0 ||
		!strings.Contains(o.Detail, "no target query") {
		t.Fatalf("outcome = %+v, b reads %d", o, b.changes)
	}
}

// A database that reads no effects at all is judged by its flag only.
func TestEffectTargetWithoutAReaderIsFlagOnly(t *testing.T) {
	a := newTarget("a", canarySQL)
	o, _ := measureMember(t, map[string]CanaryTarget{"a": a}, []string{"a"}, "a")
	if o.Failed || o.EffectMethod != EffectFlagOnly || o.EffectPct != nil ||
		!strings.Contains(o.Detail, "flag only") {
		t.Fatalf("outcome = %+v", o)
	}
}

// An unmeasured v2 verdict is flag only, with its reason, and no warning.
func TestEffectUnmeasuredV2IsFlagOnlyWithItsReason(t *testing.T) {
	a := newEffectTarget("a", 0, 0)
	a.effect = MemberEffect{Reason: "v2 did not measure the effect: too few calls"}
	o, logs := measureMember(t, members(a), []string{"a"}, "a")
	if o.EffectMethod != EffectFlagOnly || !strings.Contains(o.Detail, "too few calls") ||
		logs.joined() != "" {
		t.Fatalf("outcome = %+v, logs %q", o, logs.joined())
	}
}

// Error propagation: an unreadable effect is flag only, with the error in
// the detail and the log; it does not fail the measurement.
func TestEffectReadErrorIsFlagOnlyAndLogged(t *testing.T) {
	a := newEffectTarget("a", 0, 0)
	a.effectErr = errors.New("connection refused")
	o, logs := measureMember(t, members(a), []string{"a"}, "a")
	if o.EffectMethod != EffectFlagOnly || !strings.Contains(o.Detail, "connection refused") ||
		!strings.Contains(logs.joined(), "connection refused") {
		t.Fatalf("outcome = %+v, logs %q", o, logs.joined())
	}
}

// Error propagation: an unreachable untreated member is left out of the
// fleet control with its name in the detail; it never counts as flat.
func TestEffectUnreachableUntreatedMemberIsLeftOut(t *testing.T) {
	a, b, d := newEffectTarget("a", 0, 6), newEffectTarget("b", 0, 2),
		newEffectTarget("d", 0, 4)
	c := newEffectTarget("c", 0, 0)
	c.changeErr = errors.New("connection refused")
	// "gone" is a target the fleet no longer resolves.
	o, _ := measureMember(t, members(a, b, c, d), []string{"a", "b", "c", "gone", "d"}, "a")
	if !near(o.ControlPct, 3) || !near(o.EffectPct, 3) {
		t.Fatalf("control %s effect %s, want the median of b and d (3); c and gone counted",
			fmtPtr(o.ControlPct), fmtPtr(o.EffectPct))
	}
	for _, want := range []string{"c (", "connection refused", "gone ("} {
		if !strings.Contains(o.Detail, want) {
			t.Fatalf("detail %q does not name %q", o.Detail, want)
		}
	}
}

// Boundary: one measured untreated member is not a fleet control (two
// are needed), even when another unreachable one would make two.
func TestEffectOneMeasuredUntreatedMemberIsNoFleetControl(t *testing.T) {
	a, b := newEffectTarget("a", 7, 0), newEffectTarget("b", 0, 0)
	c := newEffectTarget("c", 0, 0)
	c.ran = false
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.EffectMethod != EffectOwnV2 || !near(o.EffectPct, 7) || o.ControlPct != nil ||
		!strings.Contains(o.Detail, "c (ran none of the target queries)") {
		t.Fatalf("outcome = %+v", o)
	}
}

// The treated member's own change unmeasured: its own v2 effect decides.
func TestEffectUnmeasuredOwnChangeFallsBackToV2(t *testing.T) {
	a, b, c := newEffectTarget("a", 9, 0), newEffectTarget("b", 0, 1),
		newEffectTarget("c", 0, 1)
	a.ran = false
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.EffectMethod != EffectOwnV2 || !near(o.EffectPct, 9) ||
		!strings.Contains(o.Detail, "own change") {
		t.Fatalf("outcome = %+v", o)
	}
}

// A failed verdict still fails the instance, whatever the number says.
func TestEffectFailedVerdictStillFails(t *testing.T) {
	a := newEffectTarget("a", -50, -50)
	all := members(a)
	logs := &logLines{}
	svc, err := NewCanaryService(func(string) (CanaryTarget, error) { return a, nil },
		&PostgresRunStore{}, CanaryOptions{RegressionLimitPct: 10, Log: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	settle(t, all["a"], 101, "rolled_back", "revert")
	b := &canaryBackend{svc: svc, findings: map[string]int{}, targets: []string{"a"},
		actions: map[string]int64{"a": 101}}
	o, err := b.Measure(context.Background(), Instance{ID: "a"}, AppliedChange{Handle: "101"})
	if err != nil || !o.Failed || !near(o.EffectPct, -50) {
		t.Fatalf("outcome = %+v (%v), want failed with the measured effect", o, err)
	}
	if len(a.ladder) != 1 || a.ladder[0].Tier != verify.TierFleet ||
		a.ladder[0].Result != verify.LadderFailed {
		t.Fatalf("ladder = %+v, want one failed tier-4 record", a.ladder)
	}
}

// A measured member gains a tier-4 ladder record with its fleet effect.
func TestEffectRecordsTierFourOnTheMemberLadder(t *testing.T) {
	a, b, c := newEffectTarget("a", -28, -30), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 0, 0)
	measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if len(a.ladder) != 1 {
		t.Fatalf("ladder = %+v, want one record", a.ladder)
	}
	rec := a.ladder[0]
	if rec.Tier != verify.TierFleet || rec.Name != "fleet" || rec.Result != verify.LadderPassed ||
		!near(rec.Predicted, -30) || rec.Evidence["method"] != EffectFleetControl ||
		rec.Evidence["database"] != "a" {
		t.Fatalf("tier-4 record = %+v", rec)
	}
}

// Concurrency: the untreated members' histories are read in parallel; a
// barrier that needs both reads at once times out when they are serial.
func TestEffectControlsAreReadInParallel(t *testing.T) {
	a, b, c := newEffectTarget("a", 0, 0), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 0, 0)
	var arrive sync.WaitGroup
	arrive.Add(2)
	both := make(chan struct{})
	go func() { arrive.Wait(); close(both) }()
	gate := func() error {
		arrive.Done()
		select {
		case <-both:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the other control was not read at the same time")
		}
	}
	b.gate, c.gate = gate, gate
	o, _ := measureMember(t, members(a, b, c), []string{"a", "b", "c"}, "a")
	if o.EffectMethod != EffectFleetControl {
		t.Fatalf("controls read serially: %+v", o)
	}
}

// State transition: a verdict still pending at its deadline reads no
// effect and records no ladder tier.
func TestEffectPendingVerdictReadsNoEffect(t *testing.T) {
	target := &clockTarget{verdicts: monitoring(1000, [2]string{"success", "success"}),
		deadline: clockBase}
	o, _, err := measureWith(t, target, time.Minute, "101")
	if err != nil || !o.Pending || o.EffectPct != nil || o.EffectMethod != "" {
		t.Fatalf("pending outcome = %+v (%v)", o, err)
	}
}

func TestEffectMedianOfEvenAndOddCounts(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}, {[]float64{-1, -1}, -1}} {
		if got := median(c.in); got != c.want {
			t.Fatalf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
