package backtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The weekly run's decisions (who runs, when, and what is skipped) with
// fake runner and reader; the store under it is tested on PostgreSQL.

type weeklyFake struct {
	latest    Result
	hasLatest bool
	latestErr error
	runErr    error
	ran       []Config
}

func (f *weeklyFake) weekly(now time.Time) *Weekly {
	return &Weekly{Config: Config{Database: "db", Days: 90, Placebos: 300,
		MaxRuntime: time.Minute},
		Leader:   func() bool { return true },
		Withhold: func(context.Context) []string { return nil },
		Latest: func(context.Context) (Result, bool, error) {
			return f.latest, f.hasLatest, f.latestErr
		},
		Run: func(_ context.Context, c Config) (Result, error) {
			f.ran = append(f.ran, c)
			if f.runErr != nil {
				return Result{}, f.runErr
			}
			return Result{Verifier: "v2", V2: passing(), RanAt: now}, nil
		},
		Now: func() time.Time { return now }}
}

var weeklyNow = time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)

func TestWeekly_RunsWhenDue(t *testing.T) {
	f := &weeklyFake{}
	did, err := f.weekly(weeklyNow).Tick(context.Background())
	if err != nil || did != "stored: gate passed" || len(f.ran) != 1 {
		t.Fatalf("first run: %q %v, %d runs", did, err, len(f.ran))
	}
	if f.ran[0].Database != "db" || f.ran[0].Placebos != 300 || f.ran[0].Now == nil {
		t.Fatalf("run config %+v", f.ran[0])
	}
	f.latest, f.hasLatest = Result{RanAt: weeklyNow.Add(-DefaultPeriod)}, true
	if _, err := f.weekly(weeklyNow).Tick(context.Background()); err != nil ||
		len(f.ran) != 2 {
		t.Fatalf("a row exactly a week old did not rerun: %v, %d runs", err, len(f.ran))
	}
}

func TestWeekly_SkipsWhenNotDue(t *testing.T) {
	f := &weeklyFake{latest: Result{RanAt: weeklyNow.Add(-DefaultPeriod + time.Second)},
		hasLatest: true}
	did, err := f.weekly(weeklyNow).Tick(context.Background())
	if err != nil || !strings.HasPrefix(did, "skipped: last ran") || len(f.ran) != 0 {
		t.Fatalf("Tick = %q %v, %d runs", did, err, len(f.ran))
	}
}

func TestWeekly_SkipsOffTheLeaderAndUnderTheWithhold(t *testing.T) {
	f := &weeklyFake{}
	w := f.weekly(weeklyNow)
	w.Leader = func() bool { return false }
	if did, err := w.Tick(context.Background()); err != nil ||
		did != "skipped: not the leader" {
		t.Fatalf("follower Tick = %q %v", did, err)
	}
	w = f.weekly(weeklyNow)
	w.Withhold = func(context.Context) []string { return []string{"replica lag 40s"} }
	if did, err := w.Tick(context.Background()); err != nil ||
		did != "skipped: host withhold: replica lag 40s" {
		t.Fatalf("withheld Tick = %q %v", did, err)
	}
	if len(f.ran) != 0 {
		t.Fatalf("%d runs while skipped", len(f.ran))
	}
}

func TestWeekly_PropagatesErrors(t *testing.T) {
	f := &weeklyFake{latestErr: errors.New("relation sage.verifier_backtest is missing")}
	if _, err := f.weekly(weeklyNow).Tick(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "verifier_backtest is missing") ||
		!strings.Contains(err.Error(), `"db"`) {
		t.Fatalf("reader failure: %v", err)
	}
	f = &weeklyFake{runErr: ErrAlreadyRunning}
	if _, err := f.weekly(weeklyNow).Tick(context.Background()); !errors.Is(err,
		ErrAlreadyRunning) {
		t.Fatalf("run failure: %v", err)
	}
	if _, err := (&Weekly{}).Tick(context.Background()); err == nil {
		t.Fatal("a Weekly with no runner ticked")
	}
}

func TestWeekly_ReportsARefusedGate(t *testing.T) {
	f := &weeklyFake{}
	w := f.weekly(weeklyNow)
	w.Run = func(context.Context, Config) (Result, error) {
		return Result{Verifier: "v2", V2: Counts{Placebos: 300, Injected: 300}}, nil
	}
	did, err := w.Tick(context.Background())
	if err != nil || did != "stored: gate refused: detection 0% below 80%" {
		t.Fatalf("Tick = %q %v", did, err)
	}
}

// A failed run is not retried every Check: the next try waits RetryAfter
// (a day by default), and a stored run clears the wait.
func TestWeekly_FailedRunWaitsBeforeRetrying(t *testing.T) {
	f := &weeklyFake{runErr: ErrRuntimeExceeded}
	now := weeklyNow
	w := f.weekly(now)
	w.Now = func() time.Time { return now }
	tick := func() (string, error) { return w.Tick(context.Background()) }
	if _, err := tick(); !errors.Is(err, ErrRuntimeExceeded) ||
		!strings.Contains(err.Error(), "next try in 24h0m0s") || len(f.ran) != 1 {
		t.Fatalf("first run: %v, %d runs", err, len(f.ran))
	}
	now = weeklyNow.Add(DefaultRetryAfter - time.Second)
	did, err := tick()
	if err != nil || did != "skipped: the last run failed; retrying after "+
		"2026-11-16T03:00:00Z" || len(f.ran) != 1 {
		t.Fatalf("an hour later: %q %v, %d runs", did, err, len(f.ran))
	}
	f.runErr = nil
	now = weeklyNow.Add(DefaultRetryAfter)
	if did, err := tick(); err != nil || did != "stored: gate passed" || len(f.ran) != 2 {
		t.Fatalf("a day later: %q %v, %d runs", did, err, len(f.ran))
	}
	f.runErr = errSnapshot
	w.RetryAfter = time.Hour
	now = now.Add(2 * DefaultPeriod)
	if _, err := tick(); !errors.Is(err, errSnapshot) || len(f.ran) != 3 {
		t.Fatalf("a stored run did not clear the wait: %v, %d runs", err, len(f.ran))
	}
	now = now.Add(time.Hour)
	if _, err := tick(); !errors.Is(err, errSnapshot) || len(f.ran) != 4 {
		t.Fatalf("RetryAfter 1h: %v, %d runs", err, len(f.ran))
	}
}
