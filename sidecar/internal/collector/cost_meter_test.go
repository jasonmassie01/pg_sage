package collector

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// footprint-05: the collector meters the catalog statements it really
// runs (the sum per cycle, the slowest with its step, the server-side
// timeouts) over its last cycles, so self-configuration sizes the
// interval and the read deadline from them instead of a proxy scan.

var meterT0 = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func stat(ms int, err error) catalogread.ReadStat {
	return catalogread.ReadStat{SQL: "SELECT 1", Begin: time.Millisecond,
		Exec: time.Duration(ms) * time.Millisecond, Err: err}
}

func pgErr(code string) error {
	return &catalogread.PhaseError{Phase: catalogread.PhaseExecution,
		Err: &pgconn.PgError{Code: code, Message: "bounded"}}
}

// runCycle records one cycle of statements, each {step, ms, err}.
func runCycle(m *costMeter, at time.Time, reads ...meterRead) {
	m.start()
	for _, r := range reads {
		m.record(withStep(context.Background(), r.step), stat(r.ms, r.err))
	}
	m.finish(at)
}

type meterRead struct {
	step string
	ms   int
	err  error
}

func TestCostMeter_OneCycle(t *testing.T) {
	var m costMeter
	runCycle(&m, meterT0,
		meterRead{"tables", 40, nil},
		meterRead{"indexes", 900, pgErr("57014")},
		meterRead{"locks", 15, pgErr("55P03")},
		meterRead{"foreign_keys", 20, &pgconn.PgError{Code: "42P01"}})
	got, ok := m.summary()
	if !ok {
		t.Fatal("no cost after a full cycle")
	}
	want := CycleCost{Cycles: 1, P90CycleMs: 975, P90SlowestMs: 900, SlowestMs: 900,
		SlowestStep: "indexes", Timeouts: 2, TimeoutSteps: []string{"indexes", "locks"},
		LastCycle: meterT0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cost = %+v\nwant   %+v", got, want)
	}
}

func TestCostMeter_UnnamedStatementsAreAttributedToTheCycle(t *testing.T) {
	var m costMeter
	m.start()
	m.record(context.Background(), stat(70, nil))
	m.finish(meterT0)
	got, _ := m.summary()
	if got.SlowestStep != unnamedStep || got.SlowestMs != 70 {
		t.Fatalf("cost = %+v, want the slowest attributed to %q", got, unnamedStep)
	}
}

// State: nothing before the first full cycle; a skipped cycle (abandoned)
// and statements outside a cycle are not counted.
func TestCostMeter_NothingBeforeTheFirstFullCycle(t *testing.T) {
	var m costMeter
	if got, ok := m.summary(); ok || !reflect.DeepEqual(got, CycleCost{}) {
		t.Fatalf("empty meter = %+v (%v), want nothing", got, ok)
	}
	m.record(withStep(context.Background(), "tables"), stat(500, pgErr("57014")))
	m.start()
	m.record(withStep(context.Background(), "load"), stat(5, nil))
	m.abandon()
	m.finish(meterT0) // a finish after an abandon commits nothing
	if got, ok := m.summary(); ok {
		t.Fatalf("an abandoned cycle and a stray statement counted: %+v", got)
	}
	runCycle(&m, meterT0, meterRead{"tables", 12, nil})
	m.record(withStep(context.Background(), "tables"), stat(9000, pgErr("57014")))
	got, ok := m.summary()
	if !ok || got.Cycles != 1 || got.SlowestMs != 12 || got.Timeouts != 0 {
		t.Fatalf("cost = %+v (%v), want only the full cycle", got, ok)
	}
}

func TestCostMeter_EmptyCycleCounts(t *testing.T) {
	var m costMeter
	runCycle(&m, meterT0)
	got, ok := m.summary()
	if !ok || got.Cycles != 1 || got.P90CycleMs != 0 || got.SlowestStep != "" {
		t.Fatalf("empty cycle = %+v (%v), want one cycle costing nothing", got, ok)
	}
}

// Boundary: the p90 is the nearest rank over the window; the window keeps
// the last costWindow cycles, so an old timeout falls out of it.
func TestCostMeter_P90OverTheWindow(t *testing.T) {
	var m costMeter
	for i := 1; i <= 10; i++ {
		runCycle(&m, meterT0.Add(time.Duration(i)*time.Minute),
			meterRead{"tables", i * 10, nil}, meterRead{"indexes", i * 100, nil})
	}
	got, _ := m.summary()
	// Sums 110..1100: ceil(0.9*10) = 9th = 990. Slowest 100..1000: 900.
	if got.Cycles != 10 || got.P90CycleMs != 990 || got.P90SlowestMs != 900 ||
		got.SlowestMs != 1000 || got.SlowestStep != "indexes" {
		t.Fatalf("cost = %+v", got)
	}
	if !got.LastCycle.Equal(meterT0.Add(10 * time.Minute)) {
		t.Fatalf("last cycle %v", got.LastCycle)
	}
}

func TestCostMeter_WindowDropsTheOldestCycle(t *testing.T) {
	var m costMeter
	runCycle(&m, meterT0, meterRead{"indexes", 4000, pgErr("57014")})
	for i := 1; i < costWindow; i++ {
		runCycle(&m, meterT0.Add(time.Duration(i)*time.Minute), meterRead{"tables", 10, nil})
	}
	got, _ := m.summary()
	if got.Cycles != costWindow || got.Timeouts != 1 || got.SlowestMs != 4000 {
		t.Fatalf("full window = %+v, want %d cycles with the timeout", got, costWindow)
	}
	runCycle(&m, meterT0.Add(2*time.Hour), meterRead{"tables", 10, nil})
	got, _ = m.summary()
	if got.Cycles != costWindow || got.Timeouts != 0 || got.SlowestMs != 10 ||
		len(got.TimeoutSteps) != 0 {
		t.Fatalf("after one more cycle = %+v, want the oldest cycle dropped", got)
	}
}

// Nil: a nil collector and a collector before its first cycle report no
// cost and do not panic.
func TestCollectorCycleCost_NilAndFresh(t *testing.T) {
	var nilC *Collector
	if got, ok := nilC.CycleCost(); ok || got.Cycles != 0 {
		t.Fatalf("nil collector = %+v (%v)", got, ok)
	}
	c := New(nil, testConfig(), 170000, noopLog)
	if _, ok := c.CycleCost(); ok {
		t.Fatal("a collector that never ran reported a cycle cost")
	}
}

// CHECK-04: production wires the meter into every catalog statement.
func TestReaderWiresTheCostMeter(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	r := c.reader(nil)
	if r.Done == nil {
		t.Fatal("reader() does not set Done: the collector's statements are not metered")
	}
	c.meter.start()
	_, err := r.Query(withStep(context.Background(), "tables"), "SELECT 1")
	c.meter.finish(meterT0)
	got, ok := c.CycleCost()
	if err == nil || !ok || got.Cycles != 1 {
		t.Fatalf("metered begin failure: %v, cost %+v (%v)", err, got, ok)
	}
}

// A breaker-skipped cycle is not a cycle: nothing is metered.
func TestCycleSkippedByTheBreakerIsNotMetered(t *testing.T) {
	c, _ := breakerCollector(t, &fakeLoadDB{err: context.DeadlineExceeded})
	c.cycle(context.Background(), &fakeTicker{})
	if got, ok := c.CycleCost(); ok {
		t.Fatalf("a skipped cycle was metered: %+v", got)
	}
}

func TestRunStepCarriesTheStepName(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	var seen string
	st := catalogStep{name: "partitions", run: func(ctx context.Context, _ *Snapshot) error {
		seen = stepOf(ctx)
		return nil
	}}
	retries := 0
	if err := c.runStep(context.Background(), st, &Snapshot{}, &retries); err != nil ||
		seen != "partitions" {
		t.Fatalf("step saw %q (%v), want partitions", seen, err)
	}
	if stepOf(context.Background()) != "" {
		t.Fatal("a bare context names a step")
	}
}

func TestIsServerTimeout(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("boom"), false},
		{&pgconn.PgError{Code: "57014"}, true},
		{&pgconn.PgError{Code: "55P03"}, true},
		{fmt.Errorf("collect: %w", pgErr("57014")), true},
		{&pgconn.PgError{Code: "57P01"}, false},
		{context.DeadlineExceeded, false},
	} {
		if got := isServerTimeout(tc.err); got != tc.want {
			t.Errorf("isServerTimeout(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Concurrency: statements are recorded from many goroutines while the
// cost is read; every statement of the cycle is counted once.
func TestCostMeter_ConcurrentRecordAndRead(t *testing.T) {
	var m costMeter
	m.start()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = m.summary()
			}
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				m.record(withStep(context.Background(), "tables"), stat(1, pgErr("57014")))
			}
		}()
	}
	wg.Wait()
	m.finish(meterT0)
	close(stop)
	got, ok := m.summary()
	if !ok || got.P90CycleMs != 800 || got.Timeouts != 800 ||
		!reflect.DeepEqual(got.TimeoutSteps, []string{"tables"}) {
		t.Fatalf("concurrent cost = %+v (%v), want 800 statements and timeouts", got, ok)
	}
}
