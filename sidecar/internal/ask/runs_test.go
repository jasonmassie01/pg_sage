package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
)

// mcp-07b: the parts of asynchronous runs that need no database: request
// checks before any I/O, the per-database run cap, the terminal state of
// a finished run, error clipping and the run metrics.

func lazyService(t *testing.T, cfg config.AskConfig) *Service {
	t.Helper()
	s, err := New(Deps{Database: "lazydb", Pool: lazyPool(t), Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestStartRun_RefusedBeforeAnyIO(t *testing.T) {
	on := config.AskConfig{Enabled: true, MaxTokensPerQuestion: 8000}
	cases := []struct {
		name string
		cfg  config.AskConfig
		c    Caller
		r    Request
		want error
	}{
		{"disabled", config.AskConfig{}, viewer, Request{Question: "q"}, ErrDisabled},
		{"empty question", on, viewer, Request{Question: "  "}, ErrInvalid},
		{"2001 runes", on, viewer, Request{Question: strings.Repeat("q", 2001)}, ErrInvalid},
		{"8001 runes", on, viewer, Request{Question: strings.Repeat("q", 8001)}, ErrInvalid},
		{"no actor", on, Caller{}, Request{Question: "q"}, ErrInvalid},
		{"bad conversation", on, viewer, Request{Question: "q", ConversationID: "x"},
			ErrNotFound},
	}
	for _, c := range cases {
		s := lazyService(t, c.cfg)
		got, err := s.StartRun(context.Background(), c.c, c.r)
		if !errors.Is(err, c.want) || got != (RunStart{}) {
			t.Errorf("%s: StartRun = %+v, %v; want %v", c.name, got, err, c.want)
		}
		if n := s.runs.count(); n != 0 {
			t.Errorf("%s: a refused start holds %d run slots", c.name, n)
		}
	}
}

func TestPollAndCancel_InvalidIDsAreNotFound(t *testing.T) {
	s := lazyService(t, config.AskConfig{Enabled: true})
	for _, id := range []string{"", "42", "not-a-uuid", "'; DROP TABLE sage.ask_runs; --"} {
		if _, err := s.PollRun(context.Background(), viewer, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("PollRun(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := s.CancelRun(context.Background(), viewer, id); !errors.Is(err,
			ErrNotFound) {
			t.Errorf("CancelRun(%q) = %v, want ErrNotFound", id, err)
		}
	}
	id := "6f1c1a52-6d1f-4d55-9a43-6a0f3c1d2e3f"
	if _, err := s.PollRun(context.Background(), Caller{}, id); !errors.Is(err, ErrInvalid) {
		t.Errorf("PollRun without an actor = %v, want ErrInvalid", err)
	}
}

func TestNew_RunDefaults(t *testing.T) {
	s := lazyService(t, config.AskConfig{Enabled: true})
	if s.runs.limit != config.DefaultAskMaxConcurrentRuns || s.d.Instance == "" ||
		s.d.RunCtx == nil {
		t.Fatalf("run defaults: limit %d instance %q ctx %v", s.runs.limit, s.d.Instance,
			s.d.RunCtx)
	}
	s = lazyService(t, config.AskConfig{Enabled: true, MaxConcurrentRuns: 2})
	if s.runs.limit != 2 {
		t.Fatalf("limit = %d, want 2", s.runs.limit)
	}
}

func TestRunTracker_CapBoundary(t *testing.T) {
	tr := newRunTracker(4)
	for i := range 4 {
		if err := tr.add(fmt.Sprint(i), &activeRun{}); err != nil {
			t.Fatalf("run %d refused under the cap: %v", i, err)
		}
	}
	if err := tr.add("4", &activeRun{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("the fifth run = %v, want ErrBusy", err)
	}
	tr.remove("2")
	if err := tr.add("5", &activeRun{}); err != nil {
		t.Fatalf("a freed slot was not reused: %v", err)
	}
	if tr.get("2") != nil || tr.get("5") == nil || tr.count() != 4 {
		t.Fatalf("tracker state: count %d", tr.count())
	}
}

func TestRunTracker_ConcurrentAddsRespectTheCap(t *testing.T) {
	tr := newRunTracker(4)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tr.add(fmt.Sprint(i), &activeRun{}) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 4 || tr.count() != 4 {
		t.Fatalf("%d adds succeeded (count %d), want 4", ok.Load(), tr.count())
	}
}

func TestRunOutcome_States(t *testing.T) {
	store := fmt.Errorf("%w: record progress: boom", ErrUnavailable)
	cases := []struct {
		name                string
		err                 error
		cancelled, shutdown bool
		want                string
	}{
		{"finished", nil, false, false, RunDone},
		{"finished after a late cancel", nil, true, false, RunDone},
		{"cancelled", context.Canceled, true, false, RunCancelled},
		{"cancel wins over shutdown", context.Canceled, true, true, RunCancelled},
		{"shutdown", context.Canceled, false, true, RunInterrupted},
		{"store outage", store, false, false, RunFailed},
		{"deadline", context.DeadlineExceeded, false, false, RunFailed},
	}
	for _, c := range cases {
		if got := runOutcome(c.err, c.cancelled, c.shutdown); got != c.want {
			t.Errorf("%s: runOutcome = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRunError_ClippedTo500Runes(t *testing.T) {
	if got := runError(nil); got != "" {
		t.Fatalf("runError(nil) = %q", got)
	}
	long := errors.New(strings.Repeat("é", 700))
	got := runError(long)
	if n := utf8.RuneCountInString(got); n == 0 || n > MaxRunErrorRunes {
		t.Fatalf("runError kept %d runes, want 1-%d", n, MaxRunErrorRunes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("runError split a rune")
	}
}

func TestRun_TerminalAndProgressJSON(t *testing.T) {
	for state, want := range map[string]bool{RunQueued: false, RunRunning: false,
		RunDone: true, RunCancelled: true, RunInterrupted: true, RunFailed: true} {
		if (Run{State: state}).Terminal() != want {
			t.Errorf("Terminal(%s) != %v", state, want)
		}
	}
	raw, err := json.Marshal(Run{ID: "r", State: RunQueued, Progress: []RunStep{}})
	if err != nil || !strings.Contains(string(raw), `"progress":[]`) ||
		strings.Contains(string(raw), `"answer"`) {
		t.Fatalf("run JSON = %s (%v)", raw, err)
	}
}

func TestRunMetrics_CountActiveAndForget(t *testing.T) {
	db := "metricsdb-runs"
	t.Cleanup(func() { ForgetDatabase(db) })
	runStarted(db)
	runStarted(db)
	runEnded(db, RunDone)
	if got := activeRuns(db); got != 1 {
		t.Fatalf("active = %d, want 1", got)
	}
	runEnded(db, RunCancelled)
	if got := activeRuns(db); got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
	if runCount(db, RunDone) != 1 || runCount(db, RunCancelled) != 1 ||
		runCount(db, RunFailed) != 0 {
		t.Fatalf("counts = %+v", RunCounts())
	}
	ForgetDatabase(db)
	if runCount(db, RunDone) != 0 {
		t.Fatal("ForgetDatabase kept the run counters")
	}
	for _, a := range ActiveRuns() {
		if a.Database == db {
			t.Fatalf("ForgetDatabase kept the active gauge: %+v", a)
		}
	}
}

// activeRuns and runCount read one series of the run metrics.
func activeRuns(db string) int64 {
	for _, a := range ActiveRuns() {
		if a.Database == db {
			return a.Active
		}
	}
	return 0
}

func runCount(db, state string) uint64 {
	for _, c := range RunCounts() {
		if c.Database == db && c.State == state {
			return c.Count
		}
	}
	return 0
}
