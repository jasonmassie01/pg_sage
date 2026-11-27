package gameday

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// benches-01: game days and local bench runs on one shared target (the
// local game-day database, which every runtime of a fleet is given) run
// one at a time across the process (Leases) and across processes (the
// bench_target cluster lock). A refused run is not a game day: it writes
// no sre_game_days row and is not kept as the bench's last run.

// grantLock is the cluster lock of a fake target: always free.
func grantLock(context.Context, string, string) (func(), bool, error) {
	return func() {}, true, nil
}

// sharedLocal is a local provider on a fake marked target whose cluster
// key is scratch.internal:port; every Create verifies a new connection.
func sharedLocal(t *testing.T, port int) (*LocalProvider, *atomic.Int32) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://u:p@scratch.internal:%d/scratch", port)
	p := mustLocal(t, dsn, nil, refusing(prodCluster()))
	var connects atomic.Int32
	p.connect = func(context.Context, string) (targetConn, error) {
		connects.Add(1)
		return scratchCluster(), nil
	}
	return p, &connects
}

func (f *gdFixture) localRunner(p clone.Provider, faults FaultRunner) *Runner {
	f.t.Helper()
	r, err := NewRunner(Config{Database: f.database, Provider: "local",
		Interval: 7 * 24 * time.Hour, Now: func() time.Time { return f.now }},
		p, faults, f.ledger, f.store)
	if err != nil {
		f.t.Fatalf("runner: %v", err)
	}
	return r
}

func localBenchOn(t *testing.T, p clone.Provider, faults FaultRunner) *LocalBench {
	t.Helper()
	b, err := NewLocalBench(LocalBenchConfig{Database: "orders", Provider: "local"}, p,
		faults, &fakeBenchLedger{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// waitStarted waits until faults has started n runs.
func waitStarted(t *testing.T, faults *fakeFaults, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		faults.mu.Lock()
		started := len(faults.dsns) >= n
		faults.mu.Unlock()
		if started {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the fault programs never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wantBusy(t *testing.T, what string, err error, holder ...string) {
	t.Helper()
	if !errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), "target busy") {
		t.Fatalf("%s: err = %v, want ErrRunning with the target busy", what, err)
	}
	for _, h := range holder {
		if !strings.Contains(err.Error(), h) {
			t.Fatalf("%s: %q does not name the holder (%q)", what, err, h)
		}
	}
}

func wantNoGameDays(t *testing.T, r *Runner) {
	t.Helper()
	list, err := r.List(context.Background(), 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("a refused run was stored: %+v (%v)", list, err)
	}
}

// Two runtimes' game days and a local bench on one local target: while
// one runs, the others are refused with the holder named and leave no
// trace; once it ends, they run.
func TestLeaseRefusesOtherRunsOnTheSharedTarget(t *testing.T) {
	fa, fb := newGDFixture(t), newGDFixture(t)
	p, _ := sharedLocal(t, 6001)
	held := &fakeFaults{report: report(fa.now, 0), block: make(chan struct{})}
	a := fa.localRunner(p, held)
	b := fb.localRunner(p, &fakeFaults{report: report(fb.now, 0)})
	benchFaults := &fakeFaults{report: []byte(scoredReport)}
	bench := localBenchOn(t, p, benchFaults)
	done := make(chan error, 1)
	go func() { _, err := a.Run(fa.ctx); done <- err }()
	waitStarted(t, held, 1)

	_, err := b.Run(fb.ctx)
	wantBusy(t, "second runtime's game day", err, "game day ", "for "+fa.database)
	wantBusy(t, "second runtime's background game day", b.Start(fb.ctx), fa.database)
	wantNoGameDays(t, b)
	_, err = bench.Run(context.Background(), nil)
	wantBusy(t, "local bench", err, "game day ", fa.database)
	_, err = bench.Start(context.Background(), []string{"lock_blocking"})
	wantBusy(t, "background local bench", err, fa.database)
	if _, ok := bench.Last(); ok || bench.Running() {
		t.Fatalf("a refused bench run was kept (running %v)", bench.Running())
	}
	if len(benchFaults.dsns) != 0 {
		t.Fatalf("refused runs ran fault programs: %v", benchFaults.dsns)
	}

	close(held.block)
	if err := <-done; err != nil {
		t.Fatalf("the holder's game day: %v", err)
	}
	if gd, err := b.Run(fb.ctx); err != nil || gd.Status != StatusCompleted {
		t.Fatalf("game day after the holder finished = %+v (%v)", gd, err)
	}
	if run, err := bench.Run(context.Background(), nil); err != nil ||
		run.Status != StatusCompleted {
		t.Fatalf("bench run after the holder finished = %+v (%v)", run, err)
	}
}

// A background local bench holds the target until it finishes, and a
// game day refused meanwhile names it.
func TestLeaseHeldByABackgroundLocalBench(t *testing.T) {
	f := newGDFixture(t)
	p, _ := sharedLocal(t, 6002)
	faults := &fakeFaults{report: []byte(scoredReport), block: make(chan struct{})}
	bench := localBenchOn(t, p, faults)
	run, err := bench.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, faults, 1)
	r := f.localRunner(p, f.faults)
	_, err = r.Run(f.ctx)
	wantBusy(t, "game day during a local bench", err, "local bench "+run.ID, "for orders")
	wantNoGameDays(t, r)
	close(faults.block)
	deadline := time.Now().Add(5 * time.Second)
	for bench.Running() {
		if time.Now().After(deadline) {
			t.Fatal("the background bench never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gd, err := r.Run(f.ctx); err != nil || gd.Status != StatusCompleted {
		t.Fatalf("game day after the bench = %+v (%v)", gd, err)
	}
}

// countingFaults records how many fault programs run at once.
type countingFaults struct {
	report       []byte
	hold         time.Duration
	active, peak atomic.Int32
	runs         atomic.Int32
}

func (f *countingFaults) Run(context.Context, string, []string, []Binding) ([]byte, error) {
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	f.runs.Add(1)
	time.Sleep(f.hold)
	return f.report, nil
}

// Concurrent access: two runtimes' game days and a local bench, all
// started together, many times. Exactly one runs fault programs at a time
// and every refusal is a busy target.
func TestLeaseOneFaultProgramAtATimeUnderContention(t *testing.T) {
	fa, fb := newGDFixture(t), newGDFixture(t)
	p, _ := sharedLocal(t, 6003)
	faults := &countingFaults{report: report(fa.now, 0), hold: 15 * time.Millisecond}
	a, b := fa.localRunner(p, faults), fb.localRunner(p, faults)
	bench := localBenchOn(t, p, faults)
	runs := []func() error{
		func() error { _, err := a.Run(fa.ctx); return err },
		func() error { _, err := b.Run(fb.ctx); return err },
		func() error { _, err := bench.Run(context.Background(), nil); return err },
	}
	var wg sync.WaitGroup
	var ok, busy atomic.Int32
	errs := make(chan error, 64)
	for _, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				switch err := run(); {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrRunning) && strings.Contains(err.Error(),
					"target busy"):
					busy.Add(1)
				default:
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("unexpected error: %v", err)
	}
	if faults.peak.Load() != 1 || ok.Load() == 0 || int(faults.runs.Load()) != int(ok.Load()) {
		t.Fatalf("peak %d concurrent fault programs, %d ran, %d succeeded, %d busy",
			faults.peak.Load(), faults.runs.Load(), ok.Load(), busy.Load())
	}
}

// Leases are per target: another local target, or a clone provider that
// gives each run its own clone, is not held.
func TestLeaseIsPerTarget(t *testing.T) {
	f, other := newGDFixture(t), newGDFixture(t)
	p, _ := sharedLocal(t, 6004)
	held := &fakeFaults{report: report(f.now, 0), block: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := f.localRunner(p, held).Run(f.ctx); done <- err }()
	defer func() {
		close(held.block)
		if err := <-done; err != nil {
			t.Errorf("the holder's game day: %v", err)
		}
	}()
	waitStarted(t, held, 1)
	q, _ := sharedLocal(t, 6005)
	if gd, err := other.localRunner(q, other.faults).Run(other.ctx); err != nil ||
		gd.Status != StatusCompleted {
		t.Fatalf("another local target was held: %+v (%v)", gd, err)
	}
	if gd, err := other.runner().Run(other.ctx); err != nil || gd.Status != StatusCompleted {
		t.Fatalf("a clone provider's run was held: %+v (%v)", gd, err)
	}
}

// Another process holds the target (the bench_target cluster lock): the
// run is refused before anything is stored or created. A lock that cannot
// be taken refuses too (fail closed) and is not reported as busy.
func TestLeaseRefusedByTheClusterLock(t *testing.T) {
	f := newGDFixture(t)
	p, connects := sharedLocal(t, 6006)
	var calls []string
	p.lock = func(_ context.Context, dsn, name string) (func(), bool, error) {
		calls = append(calls, name+"@"+dsn)
		return nil, false, nil
	}
	r := f.localRunner(p, f.faults)
	_, err := r.Run(f.ctx)
	wantBusy(t, "target locked by another process", err, "another session",
		"scratch.internal:6006")
	if len(calls) != 1 || calls[0] != BenchTargetLock+"@"+p.dsn {
		t.Fatalf("cluster lock calls = %v", calls)
	}
	p.lock = func(context.Context, string, string) (func(), bool, error) {
		return nil, false, errors.New("connection refused")
	}
	_, err = r.Run(f.ctx)
	if err == nil || errors.Is(err, ErrRunning) || !strings.Contains(err.Error(),
		"bench target lock") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("an unavailable cluster lock: err = %v", err)
	}
	wantNoGameDays(t, r)
	if connects.Load() != 0 || len(f.faults.dsns) != 0 {
		t.Fatalf("a refused run reached the target: %d connects, faults %v",
			connects.Load(), f.faults.dsns)
	}
	var released atomic.Int32
	p.lock = func(context.Context, string, string) (func(), bool, error) {
		return func() { released.Add(1) }, true, nil
	}
	if gd, err := r.Run(f.ctx); err != nil || gd.Status != StatusCompleted {
		t.Fatalf("the in-process lease was kept after a refused lock: %+v (%v)", gd, err)
	}
	if released.Load() != 1 {
		t.Fatalf("the cluster lock was released %d times, want 1", released.Load())
	}
}
