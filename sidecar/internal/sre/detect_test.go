package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The reactive detector (M6) is the deterministic trigger for families
// without an RCA signal of their own: it samples cumulative checkpoint
// and temp-file counters and LWLock waits once per trigger poll, keeps
// a short history in memory and opens one investigation per episode.
// An episode keeps one idempotency key while it lasts; a new episode of
// the same family waits out a cooldown.

var dt0 = time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)

// seqRunner answers each probe from its queue in order (the last answer
// repeats), keeping the scripted ObservedAt.
type seqRunner struct {
	mu    sync.Mutex
	queue map[probes.ID][]probes.Result
	calls map[probes.ID]int
}

func newSeqRunner() *seqRunner {
	return &seqRunner{queue: map[probes.ID][]probes.Result{}, calls: map[probes.ID]int{}}
}

func (r *seqRunner) add(rs ...probes.Result) *seqRunner {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, res := range rs {
		r.queue[res.ProbeID] = append(r.queue[res.ProbeID], res)
	}
	return r
}

func (r *seqRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.calls[id]
	r.calls[id] = n + 1
	q := r.queue[id]
	if len(q) == 0 {
		return probes.Result{ProbeID: id, Status: probes.StatusEmpty, Reason: "no_rows",
			ObservedAt: dt0}
	}
	return q[min(n, len(q)-1)]
}

func ckptPoll(at time.Duration, timed, req int64) probes.Result {
	return probes.Result{ProbeID: probes.CheckpointActivity, Status: probes.StatusOK,
		ObservedAt: dt0.Add(at), Rows: []probes.Row{{"timed_checkpoints": timed,
			"requested_checkpoints": req, "checkpointer_stats_reset": dt0.Add(-time.Hour),
			"server_started_at": dt0.Add(-time.Hour), "wal_bytes": int64(0)}}}
}

func tempPoll(at time.Duration, bytes float64) probes.Result {
	return probes.Result{ProbeID: probes.TempFileActivity, Status: probes.StatusOK,
		ObservedAt: dt0.Add(at), Rows: []probes.Row{{"temp_files": int64(1),
			"temp_bytes": bytes, "stats_reset": nil, "server_started_at": dt0.Add(-time.Hour)}}}
}

func lwPoll(at time.Duration, event string, n int64) probes.Result {
	res := probes.Result{ProbeID: probes.LWLockWaits, Status: probes.StatusEmpty,
		ObservedAt: dt0.Add(at)}
	if n > 0 {
		res.Status = probes.StatusOK
		res.Rows = []probes.Row{{"wait_event_type": "LWLock", "wait_event": event,
			"query_id": nil, "in_current_database": true, "backends": n,
			"active_backends": n + 2}}
	}
	return res
}

// fmtLog records formatted log lines.
type fmtLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *fmtLog) logFn(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *fmtLog) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func newDetector(t *testing.T, r ProbeRunner,
	mutate ...func(*DetectorConfig)) (*ReactiveDetector, *fmtLog) {
	t.Helper()
	cfg := DefaultDetectorConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	logs := &fmtLog{}
	d, err := NewReactiveDetector(r, "orders", cfg, logs.logFn)
	if err != nil {
		t.Fatalf("NewReactiveDetector: %v", err)
	}
	return d, logs
}

func poll(t *testing.T, d *ReactiveDetector) map[TriggerKind]Trigger {
	t.Helper()
	ts, err := d.Triggers(context.Background())
	if err != nil {
		t.Fatalf("Triggers: %v", err)
	}
	out := map[TriggerKind]Trigger{}
	for _, tr := range ts {
		if _, dup := out[tr.Kind]; dup {
			t.Fatalf("two %s triggers in one poll: %+v", tr.Kind, ts)
		}
		out[tr.Kind] = tr
	}
	return out
}

func TestDetector_DefaultsAreConservative(t *testing.T) {
	c := DefaultDetectorConfig()
	if c.Window != 5*time.Minute || c.CheckpointRequested != 3 || c.TempBytes != 1<<30 ||
		c.LWLockWaiters != 8 || c.LWLockPolls != 3 || c.Cooldown != 30*time.Minute {
		t.Fatalf("defaults = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestDetector_CheckpointStormOneKeyPerEpisode(t *testing.T) {
	r := newSeqRunner().add(ckptPoll(0, 50, 10), ckptPoll(15*time.Second, 50, 12),
		ckptPoll(30*time.Second, 50, 13), ckptPoll(45*time.Second, 50, 14))
	d, _ := newDetector(t, r)
	for i := 0; i < 2; i++ {
		if got := poll(t, d); len(got) != 0 {
			t.Fatalf("poll %d fired %+v before 3 requested checkpoints", i+1, got)
		}
	}
	first := poll(t, d)[TriggerCheckpoint]
	wantKey := fmt.Sprintf("detector:checkpoint_storm:%d", dt0.Add(30*time.Second).Unix())
	if first.IdempotencyKey != wantKey || first.Kind != TriggerCheckpoint ||
		first.CaseID != "sre:detector:checkpoint_storm:orders" ||
		first.Subject != "requested checkpoints" || first.IncidentID != "" {
		t.Fatalf("trigger = %+v, want key %s", first, wantKey)
	}
	if again := poll(t, d)[TriggerCheckpoint]; again.IdempotencyKey != wantKey {
		t.Fatalf("the episode continued under a new key: %+v", again)
	}
}

// Requested checkpoints must outnumber timed ones: a busy server with a
// short timeout is not a requested-checkpoint storm.
func TestDetector_CheckpointThresholdBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		timed, req int64
		fire       bool
	}{{"two requested", 0, 2, false}, {"three requested", 0, 3, true},
		{"three requested, three timed", 3, 3, false}, {"four requested, three timed", 3, 4, true}}
	for _, c := range cases {
		r := newSeqRunner().add(ckptPoll(0, 10, 10), ckptPoll(time.Minute, 10+c.timed, 10+c.req))
		d, _ := newDetector(t, r)
		poll(t, d)
		_, fired := poll(t, d)[TriggerCheckpoint]
		if fired != c.fire {
			t.Errorf("%s: fired %v, want %v", c.name, fired, c.fire)
		}
	}
}

func TestDetector_HistoryOlderThanTheWindowIsDropped(t *testing.T) {
	r := newSeqRunner().add(ckptPoll(0, 0, 10), ckptPoll(6*time.Minute, 0, 20))
	d, _ := newDetector(t, r)
	poll(t, d)
	if got := poll(t, d); len(got) != 0 {
		t.Fatalf("growth over 6 minutes fired a 5-minute detector: %+v", got)
	}
}

// CHECK-07: a statistics reset or restart is never growth.
func TestDetector_CounterResetIsNotGrowth(t *testing.T) {
	reset := ckptPoll(time.Minute, 0, 100)
	reset.Rows[0]["checkpointer_stats_reset"] = dt0
	falling := ckptPoll(2*time.Minute, 0, 90)
	falling.Rows[0]["checkpointer_stats_reset"] = dt0
	r := newSeqRunner().add(ckptPoll(0, 0, 10), reset, falling).
		add(tempPoll(0, 1<<40), tempPoll(time.Minute, 0), tempPoll(2*time.Minute, 0))
	d, _ := newDetector(t, r)
	for i := 0; i < 3; i++ {
		if got := poll(t, d); len(got) != 0 {
			t.Fatalf("poll %d fired %+v from a reset counter", i+1, got)
		}
	}
}

func TestDetector_EpisodeEndAndCooldown(t *testing.T) {
	r := newSeqRunner().add(ckptPoll(0, 0, 0), ckptPoll(15*time.Second, 0, 3),
		ckptPoll(90*time.Second, 0, 3), ckptPoll(100*time.Second, 0, 6),
		ckptPoll(32*time.Minute, 0, 6), ckptPoll(32*time.Minute+15*time.Second, 0, 9))
	d, _ := newDetector(t, r, func(c *DetectorConfig) { c.Window = time.Minute })
	poll(t, d)
	first := poll(t, d)[TriggerCheckpoint]
	if first.IdempotencyKey == "" {
		t.Fatal("the first episode did not fire")
	}
	if got := poll(t, d); len(got) != 0 {
		t.Fatalf("a quiet window kept the episode: %+v", got)
	}
	if got := poll(t, d); len(got) != 0 {
		t.Fatalf("a new episode inside the cooldown fired: %+v", got)
	}
	poll(t, d)
	second := poll(t, d)[TriggerCheckpoint]
	if second.IdempotencyKey == "" || second.IdempotencyKey == first.IdempotencyKey {
		t.Fatalf("after the cooldown: %+v (first %+v)", second, first)
	}
}

func TestDetector_TempGrowthBoundary(t *testing.T) {
	for _, c := range []struct {
		grow float64
		fire bool
	}{{1 << 30, true}, {1<<30 - 1, false}} {
		r := newSeqRunner().add(tempPoll(0, 5<<30), tempPoll(time.Minute, 5<<30+c.grow))
		d, _ := newDetector(t, r)
		poll(t, d)
		tr, fired := poll(t, d)[TriggerTempFiles]
		if fired != c.fire {
			t.Fatalf("growth %v: fired %v", c.grow, fired)
		}
		if fired && (tr.Subject != "temp-file growth" ||
			!strings.HasPrefix(tr.IdempotencyKey, "detector:temp_file_explosion:")) {
			t.Fatalf("temp trigger = %+v", tr)
		}
	}
}

func TestDetector_LWLockNeedsConsecutivePolls(t *testing.T) {
	r := newSeqRunner().add(lwPoll(0, "WALWrite", 8), lwPoll(15*time.Second, "WALWrite", 7),
		lwPoll(30*time.Second, "WALWrite", 8), lwPoll(45*time.Second, "LockManager", 9),
		lwPoll(60*time.Second, "WALWrite", 12))
	d, _ := newDetector(t, r)
	for i := 0; i < 4; i++ {
		if got := poll(t, d); len(got) != 0 {
			t.Fatalf("poll %d fired %+v without 3 consecutive hot polls", i+1, got)
		}
	}
	tr := poll(t, d)[TriggerLWLock]
	if tr.Subject != "LWLock contention" || tr.Kind != TriggerLWLock {
		t.Fatalf("lwlock trigger = %+v", tr)
	}
}

// Waits on LWLocks the graph does not model never open an investigation
// the matcher could only call inconclusive.
func TestDetector_UnmodeledLWLockDoesNotFire(t *testing.T) {
	r := newSeqRunner().add(lwPoll(0, "ProcArray", 40), lwPoll(time.Second, "ProcArray", 40),
		lwPoll(2*time.Second, "ProcArray", 40), lwPoll(3*time.Second, "ProcArray", 40))
	d, _ := newDetector(t, r)
	for i := 0; i < 4; i++ {
		if got := poll(t, d); len(got) != 0 {
			t.Fatalf("unmodeled LWLock fired %+v", got)
		}
	}
}

// A failed probe silences only its family, and is logged once per
// family and reason, not on every poll.
func TestDetector_ProbeFailureIsIsolatedAndLoggedOnce(t *testing.T) {
	denied := probes.Result{ProbeID: probes.CheckpointActivity,
		Status: probes.StatusNoPrivilege, Reason: "insufficient_privilege", ObservedAt: dt0}
	r := newSeqRunner().add(denied).add(tempPoll(0, 0), tempPoll(time.Minute, 2<<30),
		tempPoll(2*time.Minute, 4<<30))
	d, logs := newDetector(t, r)
	poll(t, d)
	poll(t, d)
	got := poll(t, d)
	if _, ok := got[TriggerTempFiles]; !ok || len(got) != 1 {
		t.Fatalf("triggers = %+v, want only the temp family", got)
	}
	if n := logs.count("checkpoint_activity"); n != 1 {
		t.Fatalf("checkpoint probe failure logged %d times, want once", n)
	}
}

func TestDetector_InvalidConfiguration(t *testing.T) {
	r := newSeqRunner()
	cases := map[string]func(*DetectorConfig){
		"zero window":       func(c *DetectorConfig) { c.Window = 0 },
		"zero requested":    func(c *DetectorConfig) { c.CheckpointRequested = 0 },
		"negative temp":     func(c *DetectorConfig) { c.TempBytes = -1 },
		"zero waiters":      func(c *DetectorConfig) { c.LWLockWaiters = 0 },
		"zero polls":        func(c *DetectorConfig) { c.LWLockPolls = 0 },
		"negative cooldown": func(c *DetectorConfig) { c.Cooldown = -time.Second },
	}
	for name, mutate := range cases {
		cfg := DefaultDetectorConfig()
		mutate(&cfg)
		if _, err := NewReactiveDetector(r, "db", cfg, nil); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := NewReactiveDetector(nil, "db", DefaultDetectorConfig(), nil); err == nil {
		t.Error("a detector without a runner was accepted")
	}
	if _, err := NewReactiveDetector(r, "", DefaultDetectorConfig(), nil); err == nil {
		t.Error("a detector without a database name was accepted")
	}
}

func TestDetector_CanceledContext(t *testing.T) {
	d, _ := newDetector(t, newSeqRunner())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Triggers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The detector's history is shared state: concurrent polls are safe.
func TestDetector_ConcurrentPolls(t *testing.T) {
	r := newSeqRunner()
	for i := 0; i < 40; i++ {
		at := time.Duration(i) * time.Second
		r.add(ckptPoll(at, 0, int64(i)), tempPoll(at, float64(i<<28)),
			lwPoll(at, "WALWrite", 9))
	}
	d, _ := newDetector(t, r)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := d.Triggers(context.Background()); err != nil {
					t.Errorf("Triggers: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

type fixedSource struct {
	ts  []Trigger
	err error
}

func (f fixedSource) Triggers(context.Context) ([]Trigger, error) { return f.ts, f.err }

func TestCombineTriggers(t *testing.T) {
	logs := &fmtLog{}
	a, b := Trigger{CaseID: "a"}, Trigger{CaseID: "b"}
	src := CombineTriggers(logs.logFn, fixedSource{ts: []Trigger{a}},
		fixedSource{err: errors.New("meta db down")}, nil, fixedSource{ts: []Trigger{b}})
	got, err := src.Triggers(context.Background())
	if err != nil || len(got) != 2 || got[0].CaseID != "a" || got[1].CaseID != "b" {
		t.Fatalf("combined = %+v (%v), want a then b", got, err)
	}
	if logs.count("meta db down") != 1 {
		t.Fatalf("a failing source was not logged: %v", logs.lines)
	}
	all := CombineTriggers(logs.logFn, fixedSource{err: errors.New("x1")},
		fixedSource{err: errors.New("x2")})
	if _, err := all.Triggers(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "x1") || !strings.Contains(err.Error(), "x2") {
		t.Fatalf("all sources failing: err = %v", err)
	}
	if ts, err := CombineTriggers(nil).Triggers(context.Background()); err != nil || ts != nil {
		t.Fatalf("no sources = %+v (%v)", ts, err)
	}
}
