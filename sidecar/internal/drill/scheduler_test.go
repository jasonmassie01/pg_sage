package drill

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-01a scheduler: one drill per database at a time, at most
// MaxConcurrent per fleet, cadence from the policy document, full copies
// only with both opt-ins, and the shortened cadence when the agent gate
// needs a fresh drill.

var schedNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

type fakeHistory struct {
	mu       sync.Mutex
	attempts map[string]Attempt
	err      error
}

func (f *fakeHistory) LastAttempt(_ context.Context, db string) (Attempt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Attempt{}, false, f.err
	}
	a, ok := f.attempts[db]
	return a, ok, nil
}

func cheapTarget(db string, run func(context.Context)) Target {
	return Target{Database: db, Enabled: true, Kind: clone.KindBranch, Run: run}
}

func TestTarget_CadenceDefaultsAndGateShortening(t *testing.T) {
	day := 24 * time.Hour
	if got := (Target{Kind: clone.KindBranch}).Cadence(); got != DefaultCadenceDays*day {
		t.Fatalf("default cadence = %s, want %d days", got, DefaultCadenceDays)
	}
	if got := (Target{Kind: clone.KindBranch, CadenceDays: 7}).Cadence(); got != 7*day {
		t.Fatalf("cadence = %s, want 7 days", got)
	}
	gated := Target{Kind: clone.KindBranch, GateProduction: true, RequireDrillDays: 14}
	if got := gated.Cadence(); got != 12*day {
		t.Fatalf("gated cheap cadence = %s, want require_restore_drill_days - 2 = 12 days", got)
	}
	gated.CadenceDays = 5
	if got := gated.Cadence(); got != 5*day {
		t.Fatalf("a shorter policy cadence = %s, want it kept (5 days)", got)
	}
	full := Target{Kind: clone.KindFullCopy, GateProduction: true, RequireDrillDays: 14}
	if got := full.Cadence(); got != DefaultCadenceDays*day {
		t.Fatalf("a full copy is never shortened: %s", got)
	}
	tiny := Target{Kind: clone.KindBranch, GateProduction: true, RequireDrillDays: 2}
	if got := tiny.Cadence(); got != day {
		t.Fatalf("require 2 days = %s, want at least 1 day", got)
	}
}

func TestTarget_KindOptIns(t *testing.T) {
	cases := []struct {
		name string
		t    Target
		ok   bool
	}{
		{"branch by default", Target{Kind: clone.KindBranch}, true},
		{"copy on write by default", Target{Kind: clone.KindCopyOnWrite}, true},
		{"thin clone by default", Target{Kind: clone.KindThinClone}, true},
		{"full copy needs both", Target{Kind: clone.KindFullCopy}, false},
		{"full copy, clone opt-in only", Target{Kind: clone.KindFullCopy,
			AllowFullCopy: true}, false},
		{"full copy, kinds only", Target{Kind: clone.KindFullCopy,
			Kinds: []clone.Kind{clone.KindFullCopy}}, false},
		{"full copy, both", Target{Kind: clone.KindFullCopy, AllowFullCopy: true,
			Kinds: []clone.Kind{clone.KindFullCopy}}, true},
		{"kinds list drops branch", Target{Kind: clone.KindBranch,
			Kinds: []clone.Kind{clone.KindCopyOnWrite}}, false},
		{"schema only never", Target{Kind: clone.KindSchemaOnly,
			Kinds: []clone.Kind{clone.KindSchemaOnly}}, false},
		{"unknown kind never", Target{Kind: ""}, false},
	}
	for _, c := range cases {
		if err := c.t.KindAllowed(); (err == nil) != c.ok {
			t.Errorf("%s: KindAllowed = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestTarget_DueBoundaries(t *testing.T) {
	tg := Target{Database: "a", Enabled: true, Kind: clone.KindBranch, CadenceDays: 30}
	if !tg.Due(Attempt{}, false, schedNow) {
		t.Fatal("a never-drilled database is due")
	}
	old := Attempt{RequestedAt: schedNow.Add(-30 * 24 * time.Hour), Passed: true}
	if !tg.Due(old, true, schedNow) {
		t.Fatal("a pass exactly one cadence old is due")
	}
	fresh := Attempt{RequestedAt: schedNow.Add(-30*24*time.Hour + time.Second), Passed: true}
	if tg.Due(fresh, true, schedNow) {
		t.Fatal("a pass younger than the cadence is not due")
	}
	failed := Attempt{RequestedAt: schedNow.Add(-FailedRetryAfter), Passed: false}
	if !tg.Due(failed, true, schedNow) {
		t.Fatal("a failed cheap drill is retried after FailedRetryAfter")
	}
	failed.RequestedAt = schedNow.Add(-FailedRetryAfter + time.Second)
	if tg.Due(failed, true, schedNow) {
		t.Fatal("a failed drill is not retried before FailedRetryAfter")
	}
	tg.Enabled = false
	if tg.Due(Attempt{}, false, schedNow) {
		t.Fatal("a disabled drill is never due")
	}
}

// blockingRuns counts started drills and holds each until release closes.
type blockingRuns struct {
	mu      sync.Mutex
	started []string
	release chan struct{}
}

func (b *blockingRuns) run(db string) func(context.Context) {
	return func(context.Context) {
		b.mu.Lock()
		b.started = append(b.started, db)
		b.mu.Unlock()
		<-b.release
	}
}

func TestScheduler_TwoPassesRespectTheFleetCap(t *testing.T) {
	runs := &blockingRuns{release: make(chan struct{})}
	s := &Scheduler{History: &fakeHistory{}, Now: func() time.Time { return schedNow }}
	var targets []Target
	for i := 0; i < 5; i++ {
		db := fmt.Sprintf("db%d", i)
		targets = append(targets, cheapTarget(db, runs.run(db)))
	}
	var wg sync.WaitGroup
	results := make([][]string, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.Pass(context.Background(), targets)
		}(i)
	}
	wg.Wait()
	all := append(append([]string{}, results[0]...), results[1]...)
	if len(all) != DefaultMaxConcurrent {
		t.Fatalf("started %v, want the fleet cap of %d", all, DefaultMaxConcurrent)
	}
	sort.Strings(all)
	if all[0] == all[1] {
		t.Fatalf("one database started twice: %v", all)
	}
	if got := s.Running(); len(got) != DefaultMaxConcurrent {
		t.Fatalf("running = %v", got)
	}
	if again := s.Pass(context.Background(), targets); len(again) != 0 {
		t.Fatalf("a third pass at the cap started %v", again)
	}
	close(runs.release)
	s.Wait()
	if got := s.Running(); len(got) != 0 {
		t.Fatalf("after the drills finished, running = %v", got)
	}
}

func TestScheduler_SkipsWhatIsNotDueOrNotAllowed(t *testing.T) {
	runs := &blockingRuns{release: make(chan struct{})}
	close(runs.release)
	hist := &fakeHistory{attempts: map[string]Attempt{
		"fresh": {RequestedAt: schedNow.Add(-time.Hour), Passed: true}}}
	s := &Scheduler{History: hist, MaxConcurrent: 5, Now: func() time.Time { return schedNow }}
	full := cheapTarget("full", runs.run("full"))
	full.Kind = clone.KindFullCopy
	off := cheapTarget("off", runs.run("off"))
	off.Enabled = false
	got := s.Pass(context.Background(), []Target{cheapTarget("fresh", runs.run("fresh")),
		full, off, cheapTarget("due", runs.run("due"))})
	s.Wait()
	if len(got) != 1 || got[0] != "due" {
		t.Fatalf("started %v, want only due", got)
	}
}

func TestScheduler_HistoryErrorStartsNothing(t *testing.T) {
	runs := &blockingRuns{release: make(chan struct{})}
	close(runs.release)
	s := &Scheduler{History: &fakeHistory{err: errors.New("connection refused")},
		Now: func() time.Time { return schedNow }}
	var logged []string
	s.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	got := s.Pass(context.Background(), []Target{cheapTarget("a", runs.run("a"))})
	if len(got) != 0 {
		t.Fatalf("started %v without history", got)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %v, want one line naming the error", logged)
	}
}

func TestScheduler_EmptyAndNilInputs(t *testing.T) {
	s := &Scheduler{History: &fakeHistory{}}
	if got := s.Pass(context.Background(), nil); len(got) != 0 {
		t.Fatalf("no targets started %v", got)
	}
	noRun := cheapTarget("a", nil)
	if got := s.Pass(context.Background(), []Target{noRun}); len(got) != 0 {
		t.Fatalf("a target without a run func started: %v", got)
	}
	var nilSched *Scheduler
	if got := nilSched.Pass(context.Background(), []Target{noRun}); got != nil {
		t.Fatalf("nil scheduler started %v", got)
	}
}
