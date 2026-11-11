package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// firstlook-05: the operator-started first look re-check, without a
// database. The database behaviour is in first_look_recheck_db_test.go.

func quietRun() *firstLookRun {
	return &firstLookRun{name: "app", logf: func(string, string, ...any) {}}
}

// Two simultaneous re-checks (past the API limiter) run one pass and
// share its result.
func TestRecheckFlight_ConcurrentCallersShareOnePass(t *testing.T) {
	var fl recheckFlight
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	pass := func() (firstlook.Report, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return firstlook.Report{ID: 41, Database: "app"}, nil
	}
	var wg sync.WaitGroup
	got := make([]firstlook.Report, 2)
	errs := make([]error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); got[0], errs[0] = fl.do(context.Background(), pass) }()
	<-started
	wg.Add(1)
	go func() { defer wg.Done(); got[1], errs[1] = fl.do(context.Background(), pass) }()
	// The second caller joins the pass in flight; give it time to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for !fl.inFlight() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("passes = %d, want 1 shared by both callers", n)
	}
	for i := range got {
		if errs[i] != nil || got[i].ID != 41 {
			t.Fatalf("caller %d: report %d err %v, want report 41", i, got[i].ID, errs[i])
		}
	}
}

// Once a pass is over, the next re-check runs a new one.
func TestRecheckFlight_SequentialCallsRunAgain(t *testing.T) {
	var fl recheckFlight
	var calls atomic.Int32
	pass := func() (firstlook.Report, error) {
		return firstlook.Report{ID: int64(calls.Add(1))}, nil
	}
	a, errA := fl.do(context.Background(), pass)
	b, errB := fl.do(context.Background(), pass)
	if errA != nil || errB != nil || a.ID != 1 || b.ID != 2 {
		t.Fatalf("reports %d, %d (errs %v, %v), want 1 then 2", a.ID, b.ID, errA, errB)
	}
	if fl.inFlight() {
		t.Fatal("a finished pass is still in flight")
	}
}

// A caller whose own request ends stops waiting with its context's error;
// the shared pass is not cancelled and its result reaches the others.
func TestRecheckFlight_WaiterContextEndsAlone(t *testing.T) {
	var fl recheckFlight
	release := make(chan struct{})
	pass := func() (firstlook.Report, error) {
		<-release
		return firstlook.Report{ID: 7}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fl.do(ctx, pass); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter err = %v, want context.Canceled", err)
	}
	done := make(chan firstlook.Report, 1)
	go func() {
		r, _ := fl.do(context.Background(), pass)
		done <- r
	}()
	close(release)
	select {
	case r := <-done:
		if r.ID != 7 {
			t.Fatalf("shared report = %d, want 7", r.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never finished after its first caller left")
	}
}

// A pass error reaches every caller of that pass.
func TestRecheckFlight_ErrorIsShared(t *testing.T) {
	var fl recheckFlight
	boom := errors.New("catalog read failed")
	_, err := fl.do(context.Background(), func() (firstlook.Report, error) {
		return firstlook.Report{}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the pass error", err)
	}
}

// Without a pool the re-check fails with ErrNoPool, after it asked the
// collector to re-probe its capabilities.
func TestRecheck_NoPoolReturnsErrNoPoolAfterTheReprobe(t *testing.T) {
	run := quietRun()
	var reprobes atomic.Int32
	run.reprobe = func(context.Context) { reprobes.Add(1) }
	_, err := run.Recheck(context.Background())
	if !errors.Is(err, firstlook.ErrNoPool) {
		t.Fatalf("err = %v, want firstlook.ErrNoPool", err)
	}
	if n := reprobes.Load(); n != 1 {
		t.Fatalf("reprobes = %d, want 1", n)
	}
}

// A re-check waits for a pass already running (the startup first look)
// before it re-probes or reads anything.
func TestRecheck_WaitsForThePassInProgress(t *testing.T) {
	run := quietRun()
	var reprobes atomic.Int32
	run.reprobe = func(context.Context) { reprobes.Add(1) }
	if err := run.gate.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := run.Recheck(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("re-check returned %v while the first look held the pass", err)
	case <-time.After(100 * time.Millisecond):
	}
	if n := reprobes.Load(); n != 0 {
		t.Fatalf("reprobes = %d while waiting, want 0", n)
	}
	run.gate.release()
	select {
	case err := <-done:
		if !errors.Is(err, firstlook.ErrNoPool) {
			t.Fatalf("err = %v, want firstlook.ErrNoPool once the pass was free", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("re-check never ran after the pass was released")
	}
}

// A caller that gives up while the pass is busy gets its context's error.
func TestRecheck_CallerContextEndsWhileWaiting(t *testing.T) {
	run := quietRun()
	if err := run.gate.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer run.gate.release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := run.Recheck(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// A stopped runtime runs no re-check.
func TestRecheck_StoppedRuntimeRunsNothing(t *testing.T) {
	run := quietRun()
	life, stop := context.WithCancel(context.Background())
	stop()
	run.life = life
	var reprobes atomic.Int32
	run.reprobe = func(context.Context) { reprobes.Add(1) }
	if _, err := run.Recheck(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled from the stopped runtime", err)
	}
	if n := reprobes.Load(); n != 0 {
		t.Fatalf("reprobes = %d on a stopped runtime, want 0", n)
	}
}

// The pass gate starts open, admits one holder at a time and honours the
// waiter's context.
func TestPassGate_OneHolderAtATime(t *testing.T) {
	var g passGate
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire err = %v, want DeadlineExceeded", err)
	}
	g.release()
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	g.release()
}

func fpItem(rule, object, severity string) firstlook.Item {
	return firstlook.Item{Rule: rule, Object: object, Severity: severity,
		Title: "t " + object, Detail: "d"}
}

func TestSummaryFingerprint(t *testing.T) {
	a := fpItem(firstlook.RuleDuplicateIndex, "public.i1", firstlook.SeverityWarning)
	b := fpItem(firstlook.RuleNeverScannedIndex, "public.i2", firstlook.SeverityInfo)
	base := firstlook.Report{Items: []firstlook.Item{a, b}}
	if summaryFingerprint(firstlook.Report{}) != "" {
		t.Fatalf("empty report fingerprint = %q, want empty",
			summaryFingerprint(firstlook.Report{}))
	}
	same := firstlook.Report{Items: []firstlook.Item{b, a}}
	if summaryFingerprint(base) != summaryFingerprint(same) {
		t.Fatal("item order changed the fingerprint")
	}
	reworded := a
	reworded.Title, reworded.Detail = "other title", "other detail"
	if summaryFingerprint(base) !=
		summaryFingerprint(firstlook.Report{Items: []firstlook.Item{reworded, b}}) {
		t.Fatal("title or detail changed the fingerprint, want rule, object, severity only")
	}
	for name, r := range map[string]firstlook.Report{
		"severity": {Items: []firstlook.Item{a, fpItem(b.Rule, b.Object,
			firstlook.SeverityWarning)}},
		"object":  {Items: []firstlook.Item{a, fpItem(b.Rule, "public.i3", b.Severity)}},
		"removed": {Items: []firstlook.Item{a}},
		"degraded check": {Items: []firstlook.Item{a, b}, Checks: []firstlook.Check{
			{Rule: firstlook.RuleSequenceRunway, Status: firstlook.CheckDegraded}}},
	} {
		if summaryFingerprint(r) == summaryFingerprint(base) {
			t.Errorf("%s change kept the fingerprint", name)
		}
	}
	ok := firstlook.Report{Items: []firstlook.Item{a, b}, Checks: []firstlook.Check{
		{Rule: firstlook.RuleSequenceRunway, Status: firstlook.CheckOK}}}
	if summaryFingerprint(ok) != summaryFingerprint(base) {
		t.Fatal("a check that ran changed the fingerprint")
	}
}

// A runtime without a first look exposes no re-check hook (the API then
// answers 503).
func TestRuntimeFirstLookRecheck_NilWithoutAFirstLook(t *testing.T) {
	if hook := (&databaseRuntime{}).firstLookRecheck(); hook != nil {
		t.Fatal("hook set on a runtime with no first look")
	}
	rt := &databaseRuntime{firstLook: quietRun()}
	hook := rt.firstLookRecheck()
	if hook == nil {
		t.Fatal("no hook on a runtime with a first look")
	}
	if _, err := hook(context.Background()); !errors.Is(err, firstlook.ErrNoPool) {
		t.Fatalf("hook err = %v, want the run's ErrNoPool", err)
	}
}
