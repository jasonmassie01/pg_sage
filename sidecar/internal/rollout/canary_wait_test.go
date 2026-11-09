package rollout

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// executor-04: a canary measurement waits for the target's own verdict.
// A reversible action stays "monitoring" for its monitor window, so the
// measurement polls until the verdict is terminal, bounded by the
// target's monitor deadline plus a grace; at that bound it reports the
// verdict as pending instead of judging it.

// clockTarget is a CanaryTarget whose verdicts are a script read one per
// poll, on a fake clock the service's wait advances.
type clockTarget struct {
	mu        sync.Mutex
	verdicts  [][2]string // one per ActionResult call; the last repeats
	polls     int
	deadline  time.Time
	deadErr   error
	resultErr error
	readAt    []time.Time
	now       func() time.Time
}

func (c *clockTarget) Name() string { return "clock" }
func (c *clockTarget) SourceAction(context.Context, int64) (SourceAction, error) {
	return SourceAction{}, errors.New("not a source")
}
func (c *clockTarget) MatchingFinding(context.Context, string) (int, bool, error) {
	return 0, false, nil
}
func (c *clockTarget) Execute(context.Context, int, string, string, *int) (int64, error) {
	return 0, errors.New("not used")
}
func (c *clockTarget) Rollback(context.Context, int64, string) error { return nil }

func (c *clockTarget) ActionResult(context.Context, int64) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readAt = append(c.readAt, c.now())
	if c.resultErr != nil {
		return "", "", c.resultErr
	}
	i := min(c.polls, len(c.verdicts)-1)
	c.polls++
	return c.verdicts[i][0], c.verdicts[i][1], nil
}

func (c *clockTarget) MonitorDeadline(context.Context, int64) (time.Time, error) {
	return c.deadline, c.deadErr
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

var clockBase = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// measureWith runs one Measure against target on a fake clock that each
// wait advances by the waited duration.
func measureWith(t *testing.T, target *clockTarget, poll time.Duration,
	handle string) (Outcome, []time.Duration, error) {
	t.Helper()
	clock := &fakeClock{now: clockBase}
	target.now = clock.Now
	svc, err := NewCanaryService(func(string) (CanaryTarget, error) { return target, nil },
		&PostgresRunStore{}, CanaryOptions{PollEvery: poll, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	svc.wait = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, d)
		clock.advance(d)
		return nil
	}
	b := &canaryBackend{svc: svc, findings: map[string]int{}, actions: map[string]int64{}}
	o, err := b.Measure(context.Background(), Instance{ID: "a"}, AppliedChange{Handle: handle})
	return o, waits, err
}

func monitoring(n int, final [2]string) [][2]string {
	out := make([][2]string, 0, n+1)
	for i := 0; i < n; i++ {
		out = append(out, [2]string{"monitoring", "pending"})
	}
	return append(out, final)
}

func TestMeasureWaitsForTheTargetVerdict(t *testing.T) {
	target := &clockTarget{verdicts: monitoring(3, [2]string{"success", "success"}),
		deadline: clockBase.Add(15 * time.Minute)}
	o, waits, err := measureWith(t, target, 15*time.Second, "101")
	if err != nil {
		t.Fatal(err)
	}
	if o.Failed || o.Pending || o.RegressionPct != 0 || o.EvidenceID != "action_log:101" {
		t.Fatalf("outcome = %+v, want a judged success", o)
	}
	if target.polls != 4 || len(waits) != 3 {
		t.Fatalf("polls = %d, waits = %v; want 4 polls and 3 waits", target.polls, waits)
	}
	for _, w := range waits {
		if w != 15*time.Second {
			t.Fatalf("waits = %v, want every wait to be the 15 s poll", waits)
		}
	}
	if !strings.Contains(o.Detail, "outcome success") {
		t.Fatalf("detail = %q", o.Detail)
	}
}

func TestMeasureDefaultPollIsFifteenSeconds(t *testing.T) {
	target := &clockTarget{verdicts: monitoring(1, [2]string{"success", "success"}),
		deadline: clockBase.Add(time.Hour)}
	_, waits, err := measureWith(t, target, 0, "5")
	if err != nil || len(waits) != 1 || waits[0] != 15*time.Second {
		t.Fatalf("waits = %v (%v), want one 15 s wait", waits, err)
	}
}

func TestMeasureFailedVerdictsFailAfterWaiting(t *testing.T) {
	for name, final := range map[string][2]string{
		"revert":       {"rolled_back", "revert"},
		"failed":       {"failed", "failed"},
		"unverifiable": {"unverifiable", "unverifiable"},
		"rollback":     {"rollback_failed", ""},
		"neutral kept": {"success", "unverifiable"},
	} {
		target := &clockTarget{verdicts: monitoring(2, final),
			deadline: clockBase.Add(15 * time.Minute)}
		o, _, err := measureWith(t, target, 15*time.Second, "7")
		if err != nil || !o.Failed || o.Pending || o.RegressionPct != 100 {
			t.Errorf("%s: outcome = %+v (%v), want failed at 100%%", name, o, err)
		}
		if target.polls != 3 {
			t.Errorf("%s: polls = %d, want 3 (it waited for the verdict)", name, target.polls)
		}
	}
}

// A success outcome is terminal even before the verification row lands.
func TestMeasureSuccessOutcomeIsTerminal(t *testing.T) {
	target := &clockTarget{verdicts: monitoring(1, [2]string{"success", ""}),
		deadline: clockBase.Add(15 * time.Minute)}
	o, _, err := measureWith(t, target, 15*time.Second, "8")
	if err != nil || o.Failed || o.Pending || target.polls != 2 {
		t.Fatalf("outcome = %+v (%v), polls %d", o, err, target.polls)
	}
}

// Boundary: the wait ends exactly at the monitor deadline plus 2 minutes,
// never earlier, and the last read happens at that instant.
func TestMeasureDeadlineIsTheMonitorCapPlusTwoMinutes(t *testing.T) {
	capEnd := clockBase.Add(15*time.Minute + 7*time.Second)
	target := &clockTarget{verdicts: [][2]string{{"monitoring", "pending"}}, deadline: capEnd}
	o, waits, err := measureWith(t, target, 15*time.Second, "9")
	if err != nil {
		t.Fatal(err)
	}
	if !o.Pending || o.Failed || o.RegressionPct != 0 {
		t.Fatalf("outcome = %+v, want pending without a judgement", o)
	}
	want := capEnd.Add(2 * time.Minute)
	last := target.readAt[len(target.readAt)-1]
	if !last.Equal(want) {
		t.Fatalf("last read at %v, want exactly %v", last, want)
	}
	for _, at := range target.readAt[:len(target.readAt)-1] {
		if !at.Before(want) {
			t.Fatalf("read at %v, at or after the deadline before the last read", at)
		}
	}
	var total time.Duration
	for _, w := range waits {
		total += w
	}
	if total != want.Sub(clockBase) {
		t.Fatalf("waited %v in all, want %v", total, want.Sub(clockBase))
	}
	if !strings.Contains(o.Detail, "monitoring") {
		t.Fatalf("detail = %q, want it to say the target is still monitoring", o.Detail)
	}
}

// A verdict that lands on the very last read is judged, not pending.
func TestMeasureVerdictAtTheDeadlineIsJudged(t *testing.T) {
	// deadline now + 2 min grace, 15 s polls: reads at 0, 15, ... 120 s (9 reads).
	target := &clockTarget{verdicts: monitoring(8, [2]string{"success", "success"}),
		deadline: clockBase}
	o, _, err := measureWith(t, target, 15*time.Second, "10")
	if err != nil || o.Pending || o.Failed || target.polls != 9 {
		t.Fatalf("outcome = %+v (%v), polls %d; want a judged success on read 9", o, err,
			target.polls)
	}
}

// A deadline already past still reads the verdict once before reporting
// it pending.
func TestMeasurePastDeadlineReadsOnce(t *testing.T) {
	target := &clockTarget{verdicts: [][2]string{{"monitoring", ""}},
		deadline: clockBase.Add(-time.Hour)}
	o, waits, err := measureWith(t, target, 15*time.Second, "11")
	if err != nil || !o.Pending || target.polls != 1 || len(waits) != 0 {
		t.Fatalf("outcome = %+v (%v), polls %d, waits %v", o, err, target.polls, waits)
	}
}

func TestMeasurePropagatesTargetErrors(t *testing.T) {
	target := &clockTarget{verdicts: monitoring(0, [2]string{"success", "success"}),
		deadErr: errors.New("action 12 not found")}
	if _, _, err := measureWith(t, target, time.Second, "12"); err == nil ||
		!strings.Contains(err.Error(), "action 12 not found") {
		t.Fatalf("deadline error = %v", err)
	}
	target = &clockTarget{resultErr: errors.New("connection refused"),
		deadline: clockBase.Add(time.Hour)}
	if _, _, err := measureWith(t, target, time.Second, "13"); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("result error = %v", err)
	}
}

// Invalid input: a change handle that is not an action id is an error,
// never a read of action 0.
func TestMeasureRejectsABadHandle(t *testing.T) {
	for _, handle := range []string{"", "abc", "0", "-4"} {
		target := &clockTarget{verdicts: monitoring(0, [2]string{"success", "success"}),
			deadline: clockBase.Add(time.Hour)}
		_, _, err := measureWith(t, target, time.Second, handle)
		if err == nil || target.polls != 0 {
			t.Errorf("handle %q: err = %v, polls %d", handle, err, target.polls)
		}
	}
}

func TestMeasureStopsWhenTheContextEnds(t *testing.T) {
	target := &clockTarget{verdicts: [][2]string{{"monitoring", ""}},
		deadline: clockBase.Add(time.Hour), now: time.Now}
	svc, err := NewCanaryService(func(string) (CanaryTarget, error) { return target, nil },
		&PostgresRunStore{}, CanaryOptions{PollEvery: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &canaryBackend{svc: svc, findings: map[string]int{}, actions: map[string]int64{}}
	_, err = b.Measure(ctx, Instance{ID: "a"}, AppliedChange{Handle: "3"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Nil/empty: zero targets is still rejected before anything is read.
func TestValidateStartRejectsZeroTargets(t *testing.T) {
	for _, targets := range [][]string{nil, {}} {
		err := validateStart(StartRequest{SourceDatabase: "src", ActionLogID: 1,
			Targets: targets, StartedBy: "user:1:a@example.com"})
		if !errors.Is(err, ErrInvalidCanary) || !strings.Contains(err.Error(), "1-100") {
			t.Fatalf("targets %v: err = %v", targets, err)
		}
	}
}

// No concurrent-access test here: Measure keeps no state between polls;
// single-flight runs are covered by TestCanaryStartIsAsyncAndSingleFlight.
