package selfbudget

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink models each database's open sidecar CPU finding.
type fakeSink struct {
	mu       sync.Mutex
	dbs      []string
	open     map[string]Breach
	raises   map[string]int
	clears   map[string]int
	failOn   map[string]error
	clearErr map[string]error
	loops    []LoopCost
}

func newSink(dbs ...string) *fakeSink {
	return &fakeSink{dbs: dbs, open: map[string]Breach{}, raises: map[string]int{},
		clears: map[string]int{}, failOn: map[string]error{}, clearErr: map[string]error{}}
}

func (s *fakeSink) Databases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dbs...)
}

func (s *fakeSink) setDatabases(dbs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbs = dbs
}

func (s *fakeSink) Raise(_ context.Context, db string, b Breach, loops []LoopCost) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failOn[db]; err != nil {
		return err
	}
	s.open[db], s.loops = b, loops
	s.raises[db]++
	return nil
}

func (s *fakeSink) Clear(_ context.Context, db string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.clearErr[db]; err != nil {
		return err
	}
	delete(s.open, db)
	s.clears[db]++
	return nil
}

func (s *fakeSink) openDatabases() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.open))
	for db := range s.open {
		out = append(out, db)
	}
	sort.Strings(out)
	return out
}

type watchRig struct {
	cpu    *fakeCPU
	sink   *fakeSink
	loops  *Loops
	budget float64
	watch  *ProcessCPUWatch
}

func newWatchRig(budget float64, dbs ...string) *watchRig {
	r := &watchRig{cpu: newFake(), sink: newSink(dbs...), loops: NewLoops(), budget: budget}
	r.watch = NewProcessCPUWatch(WatchOptions{Read: r.cpu.read, Now: r.cpu.now,
		Budget: func() float64 { return r.budget },
		Cycle:  func() time.Duration { return time.Minute },
		Sink:   r.sink, Loops: r.loops})
	return r
}

// tick advances the fake process by wall time and CPU, then ticks.
func (r *watchRig) tick(t *testing.T, wall, cpu time.Duration) {
	t.Helper()
	r.cpu.advance(wall, cpu)
	if err := r.watch.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

// footprint-07 concurrency: three analyzers' loops run beside one watch
// over one fake CPU reader; the process CPU breach is one finding, on the
// lowest-named live database, however the ticks interleave.
func TestProcessCPUWatch_ThreeAnalyzersOneFinding(t *testing.T) {
	r := newWatchRig(1, "db3", "db1", "db2")
	r.tick(t, 0, 0) // first observation: unknown
	var wg sync.WaitGroup
	for _, db := range []string{"db1", "db2", "db3"} {
		wg.Add(1)
		go func(db string) {
			defer wg.Done()
			ctx := WithDatabase(context.Background(), db)
			for i := 0; i < 50; i++ {
				_, done := r.loops.TrackFor(ctx, "analyzer")
				done()
			}
		}(db)
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				r.cpu.advance(time.Second, time.Second)
				if err := r.watch.Tick(context.Background()); err != nil {
					t.Errorf("tick: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if got := r.sink.openDatabases(); len(got) != 1 || got[0] != "db1" {
		t.Fatalf("open CPU findings on %v, want exactly db1", got)
	}
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	if r.sink.raises["db2"] != 0 || r.sink.raises["db3"] != 0 {
		t.Fatalf("raises = %v, want none outside db1", r.sink.raises)
	}
	if b := r.sink.open["db1"]; b.Resource != ResourceCPU || b.Limit != 1 || b.Used <= 1 ||
		b.Unit != unitCPU {
		t.Fatalf("breach = %+v, want cpu over a 1 ms budget", b)
	}
}

// The first observation knows no window: nothing is raised or cleared, so
// an open finding from before a restart stays open until there is proof.
func TestProcessCPUWatch_FirstTickIsUnknown(t *testing.T) {
	r := newWatchRig(1, "db1")
	r.tick(t, time.Minute, time.Hour)
	if len(r.sink.raises) != 0 || len(r.sink.clears) != 0 {
		t.Fatalf("first tick raised %v cleared %v", r.sink.raises, r.sink.clears)
	}
	if got := r.watch.Reading(); got.Known || got.MsPerCycle != 0 {
		t.Fatalf("reading = %+v, want unknown", got)
	}
}

// 1.5 s of CPU per 30 s wall is 3000 ms per 60 s cycle against a 600 ms
// budget: one breach with the reading published for /metrics and the API.
func TestProcessCPUWatch_RaisesWithTheRateAndLoops(t *testing.T) {
	r := newWatchRig(600, "db2", "db1")
	r.tick(t, 0, 0)
	ctx := WithDatabase(context.Background(), "db2")
	r.loops.Add(LoopName("collector", "db2"), 7*time.Second)
	r.loops.Add(LoopName(LLMWaitLoop, "db2"), time.Hour)
	_, done := r.loops.TrackFor(ctx, "executor")
	done()
	r.tick(t, 30*time.Second, 1500*time.Millisecond)
	got := r.watch.Reading()
	if !got.Known || got.MsPerCycle != 3000 || got.BudgetMs != 600 || got.Database != "db1" {
		t.Fatalf("reading = %+v, want 3000 of 600 on db1", got)
	}
	if b := r.sink.open["db1"]; b.Used != 3000 || b.Limit != 600 {
		t.Fatalf("breach = %+v", b)
	}
	if len(r.sink.loops) == 0 || r.sink.loops[0].Name != "collector:db2" ||
		r.sink.loops[0].BusyMs != 7000 {
		t.Fatalf("loops = %+v, want collector:db2 first", r.sink.loops)
	}
	for _, l := range append(r.sink.loops, got.Loops...) {
		if strings.HasPrefix(l.Name, LLMWaitLoop) {
			t.Fatalf("llm wait ranked as local work: %+v", l)
		}
	}
}

// Boundary: usage exactly at the budget is not a breach (strictly above).
func TestProcessCPUWatch_AtBudgetIsNotABreach(t *testing.T) {
	r := newWatchRig(600, "db1")
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, 600*time.Millisecond)
	if len(r.sink.raises) != 0 || r.sink.clears["db1"] != 1 {
		t.Fatalf("at budget: raises %v clears %v, want one clear", r.sink.raises,
			r.sink.clears)
	}
	r.tick(t, time.Minute, 601*time.Millisecond)
	if r.sink.raises["db1"] != 1 {
		t.Fatalf("1 ms over budget raised %v", r.sink.raises)
	}
}

// State transition: under budget clears every database once, then only a
// database where the finding was raised since is cleared again.
func TestProcessCPUWatch_ClearsOnlyWhatMayBeOpen(t *testing.T) {
	r := newWatchRig(600, "db1", "db2", "db3")
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, time.Millisecond)
	for _, db := range []string{"db1", "db2", "db3"} {
		if r.sink.clears[db] != 1 {
			t.Fatalf("clears = %v, want each database once", r.sink.clears)
		}
	}
	r.tick(t, time.Minute, time.Millisecond)
	if r.sink.clears["db1"] != 1 || r.sink.clears["db2"] != 1 {
		t.Fatalf("known-clean databases cleared again: %v", r.sink.clears)
	}
	r.tick(t, time.Minute, time.Hour) // breach on db1
	r.tick(t, time.Minute, time.Millisecond)
	if r.sink.clears["db1"] != 2 || r.sink.clears["db2"] != 1 || len(r.sink.openDatabases()) != 0 {
		t.Fatalf("clears = %v open = %v, want db1 cleared after its breach",
			r.sink.clears, r.sink.openDatabases())
	}
}

// The finding follows the lowest-named live database: when it leaves, the
// next one carries the finding; when it returns, the copy elsewhere is
// resolved.
func TestProcessCPUWatch_FindingMovesWithTheLowestLiveDatabase(t *testing.T) {
	r := newWatchRig(1, "db1", "db2")
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, time.Hour)
	if got := r.sink.openDatabases(); len(got) != 1 || got[0] != "db1" {
		t.Fatalf("open = %v, want db1", got)
	}
	r.sink.setDatabases("db2") // db1 disconnected: its row cannot be reached
	r.tick(t, time.Minute, time.Hour)
	if r.sink.raises["db2"] != 1 {
		t.Fatalf("raises = %v, want db2 to carry the finding", r.sink.raises)
	}
	r.sink.setDatabases("db2", "db1")
	r.tick(t, time.Minute, time.Hour)
	if got := r.sink.openDatabases(); len(got) != 1 || got[0] != "db1" {
		t.Fatalf("open = %v, want only db1 once it is back", got)
	}
}

// Error propagation: a database that refuses the write (a replica) hands
// the finding to the next; the refusal is returned with its database.
func TestProcessCPUWatch_RaiseFailureFallsBack(t *testing.T) {
	r := newWatchRig(1, "db1", "db2")
	r.sink.failOn["db1"] = errors.New("cannot execute INSERT in a read-only transaction")
	r.tick(t, 0, 0)
	r.cpu.advance(time.Minute, time.Hour)
	err := r.watch.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "db1") ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v, want db1's refusal", err)
	}
	if got := r.sink.openDatabases(); len(got) != 1 || got[0] != "db2" {
		t.Fatalf("open = %v, want db2", got)
	}
	if got := r.watch.Reading(); got.Database != "db2" {
		t.Fatalf("reading database = %q, want db2", got.Database)
	}
}

// A failed clear is returned and retried on the next tick.
func TestProcessCPUWatch_ClearFailureIsRetried(t *testing.T) {
	r := newWatchRig(600, "db1")
	r.sink.clearErr["db1"] = errors.New("connection refused")
	r.tick(t, 0, 0)
	r.cpu.advance(time.Minute, time.Millisecond)
	if err := r.watch.Tick(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "connection refused") ||
		!strings.Contains(err.Error(), "db1") {
		t.Fatalf("err = %v, want db1's clear failure", err)
	}
	delete(r.sink.clearErr, "db1")
	r.tick(t, time.Minute, time.Millisecond)
	if r.sink.clears["db1"] != 1 {
		t.Fatalf("clears = %v, want the retry", r.sink.clears)
	}
}

// Nil/empty: no databases means no finding, but the reading is still
// published for the gauge.
func TestProcessCPUWatch_NoDatabasesStillPublishesTheReading(t *testing.T) {
	r := newWatchRig(1)
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, time.Second)
	if len(r.sink.raises) != 0 || len(r.sink.clears) != 0 {
		t.Fatalf("raised %v cleared %v without databases", r.sink.raises, r.sink.clears)
	}
	if got := r.watch.Reading(); !got.Known || got.MsPerCycle != 1000 || got.Database != "" {
		t.Fatalf("reading = %+v, want 1000 ms known on no database", got)
	}
}

// A zero budget disables the resource: measured, never raised, and an
// old finding resolves.
func TestProcessCPUWatch_ZeroBudgetDisables(t *testing.T) {
	r := newWatchRig(0, "db1")
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, time.Hour)
	if len(r.sink.raises) != 0 || r.sink.clears["db1"] != 1 {
		t.Fatalf("raises %v clears %v, want one clear", r.sink.raises, r.sink.clears)
	}
	if got := r.watch.Reading(); !got.Known || got.BudgetMs != 0 {
		t.Fatalf("reading = %+v", got)
	}
}

// An unreadable counter makes the reading unknown again; the open
// finding is left alone.
func TestProcessCPUWatch_UnreadableKeepsTheFinding(t *testing.T) {
	r := newWatchRig(1, "db1")
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, time.Hour)
	r.cpu.mu.Lock()
	r.cpu.ok = false
	r.cpu.mu.Unlock()
	r.tick(t, time.Minute, time.Hour)
	if got := r.sink.openDatabases(); len(got) != 1 || r.sink.clears["db1"] != 0 {
		t.Fatalf("open %v clears %v, want the finding kept", got, r.sink.clears)
	}
	if r.watch.Reading().Known {
		t.Fatal("unreadable counter still reads as known")
	}
}

func TestProcessCPUWatch_NilIsSafe(t *testing.T) {
	var w *ProcessCPUWatch
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("nil tick: %v", err)
	}
	if got := w.Reading(); got.Known {
		t.Fatalf("nil reading = %+v", got)
	}
	empty := NewProcessCPUWatch(WatchOptions{})
	if err := empty.Tick(context.Background()); err != nil {
		t.Fatalf("watch without options: %v", err)
	}
}

func TestProcessWatch_Registry(t *testing.T) {
	prev := ProcessWatch()
	t.Cleanup(func() { SetProcessWatch(prev) })
	r := newWatchRig(1)
	SetProcessWatch(r.watch)
	if ProcessWatch() != r.watch {
		t.Fatal("ProcessWatch did not return the registered watch")
	}
	SetProcessWatch(nil)
	if ProcessWatch() != nil || ProcessWatch().Reading().Known {
		t.Fatal("cleared registry still answers")
	}
}

// Run ticks at the cycle and stops with ctx.
func TestProcessCPUWatch_RunStopsWithContext(t *testing.T) {
	r := newWatchRig(1, "db1")
	var ticks sync.WaitGroup
	ticks.Add(1)
	r.watch.cycle = func() time.Duration { return time.Millisecond }
	r.sink.failOn["db1"] = errors.New("refused")
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var once sync.Once
	go func() {
		r.watch.Run(ctx, func(error) { once.Do(ticks.Done) })
		close(finished)
	}()
	go func() {
		for ctx.Err() == nil {
			r.cpu.advance(time.Minute, time.Hour)
			time.Sleep(time.Millisecond)
		}
	}()
	ticks.Wait()
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

// Run reports every tick, a clean one as nil, so a reporter can tell a
// recovery from a repeat of the same refusal (review of footprint-07).
func TestProcessCPUWatch_RunReportsCleanTicks(t *testing.T) {
	r := newWatchRig(1, "db1")
	r.watch.cycle = func() time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 64)
	go r.watch.Run(ctx, func(err error) {
		select {
		case results <- err:
		default:
		}
	})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-results:
			if err == nil {
				return
			}
			t.Fatalf("clean sink reported %v", err)
		case <-deadline:
			t.Fatal("Run never reported a clean tick")
		}
	}
}
