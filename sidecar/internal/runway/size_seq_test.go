package runway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Size identity (forecast-11): a measurement is told apart from the last
// one by its sequence, not by the instant it was stamped. On Windows two
// clock readings close together can be equal, so a timestamp identity
// made back-to-back measurements look like one.

// keylessRunner answers wal_runway without a system identifier, so the
// reading has no cluster key and every tick measures (through the share's
// clock, never shared).
type keylessRunner struct{ *sizeCadenceRunner }

func (r keylessRunner) Run(ctx context.Context, id probes.ID, a probes.Args) probes.Result {
	res := r.sizeCadenceRunner.Run(ctx, id, a)
	if id == probes.WALRunwayProbe {
		row := probes.Row{}
		for k, v := range res.Rows[0] {
			row[k] = v
		}
		delete(row, "system_identifier")
		res.Rows = []probes.Row{row}
	}
	return res
}

func (r keylessRunner) RunBackground(ctx context.Context, id probes.ID,
	a probes.Args) probes.Result {
	return r.Run(ctx, id, a)
}

// Happy path: a frozen clock stamps three measurements with one instant;
// each is still a new measurement and each is fresh.
func TestSizeSeq_EqualStampsAreStillFresh(t *testing.T) {
	r := keylessRunner{&sizeCadenceRunner{}}
	share, clock := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	stamp := clock.Now()
	for i := 0; i < 3; i++ {
		snap := readSize(t, m)
		if !snap.SizeFresh {
			t.Fatalf("read %d at one frozen instant not fresh", i)
		}
		if got := snap.WAL.DatabaseBytes; got != float64(int64(i+1)<<20) {
			t.Fatalf("read %d: %v bytes, want measurement %d", i, got, i+1)
		}
	}
	if r.measured() != 3 {
		t.Fatalf("%d measurements, want 3", r.measured())
	}
	if !clock.Now().Equal(stamp) {
		t.Fatal("the test clock moved; the stamps were not equal")
	}
}

// Nil share: a measurement still gets a sequence, and back-to-back
// measurements get distinct, increasing ones.
func TestSizeSeq_NilShareAssignsIncreasingSequences(t *testing.T) {
	var none *SizeShare
	c := &sizeCounter{value: 1}
	a, _, errA := none.Measure(context.Background(), "k", time.Minute, c.measure)
	b, _, errB := none.Measure(context.Background(), "k", time.Minute, c.measure)
	if errA != nil || errB != nil {
		t.Fatalf("measure: %v, %v", errA, errB)
	}
	if a.seq == 0 || b.seq <= a.seq {
		t.Fatalf("sequences %d then %d, want nonzero and increasing", a.seq, b.seq)
	}
	if a.MeasuredAt.IsZero() || b.MeasuredAt.IsZero() {
		t.Fatalf("a nil share no longer stamps MeasuredAt: %v, %v", a.MeasuredAt, b.MeasuredAt)
	}
}

// Zero: a monitor that never measured has no sequence; its first
// measurement is fresh and recorded.
func TestSizeSeq_FirstMeasurementIsFresh(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, _ := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	if m.sizeSeq != 0 {
		t.Fatalf("a new monitor has sequence %d, want 0", m.sizeSeq)
	}
	if snap := readSize(t, m); !snap.SizeFresh {
		t.Fatal("the first measurement is not fresh")
	}
	m.sizeMu.Lock()
	seq := m.sizeSeq
	m.sizeMu.Unlock()
	if seq == 0 {
		t.Fatal("the monitor did not record the first measurement's sequence")
	}
}

// Error propagation: a failed measurement carries no sequence and is
// never shared.
func TestSizeSeq_FailedMeasurementHasNoSequence(t *testing.T) {
	share, _ := newTestShare()
	boom := errors.New("cluster_database_size: relation vanished")
	c := &sizeCounter{err: boom}
	for _, s := range []*SizeShare{share, nil} {
		got, shared, err := s.Measure(context.Background(), "k", time.Minute, c.measure)
		if !errors.Is(err, boom) || shared || got.seq != 0 || !got.MeasuredAt.IsZero() {
			t.Fatalf("failed measurement: %+v shared %v err %v", got, shared, err)
		}
	}
}

// Boundary and state transitions: a shared reading keeps its sequence;
// one pass later a new measurement gets a new one; freshness for one
// monitor flips exactly at each new measurement.
func TestSizeSeq_SharedReadingKeepsItsSequence(t *testing.T) {
	share, clock := newTestShare()
	c := &sizeCounter{value: 1}
	ctx := context.Background()
	first, _, _ := share.Measure(ctx, "k", time.Minute, c.measure)
	clock.advance(time.Minute - time.Nanosecond)
	again, shared, _ := share.Measure(ctx, "k", time.Minute, c.measure)
	if !shared || again.seq != first.seq {
		t.Fatalf("shared %v: sequence %d, want the first's %d", shared, again.seq, first.seq)
	}
	clock.advance(time.Nanosecond)
	next, shared, _ := share.Measure(ctx, "k", time.Minute, c.measure)
	if shared || next.seq <= first.seq {
		t.Fatalf("re-measured: shared %v, sequence %d after %d", shared, next.seq, first.seq)
	}
}

func TestSizeSeq_FreshnessFlipsAtEachMeasurement(t *testing.T) {
	r := &sizeCadenceRunner{}
	share, clock := newTestShare()
	m := sizeMonitor(r, 10*time.Minute, share)
	want := []bool{true, false, false, true, false, true}
	steps := []time.Duration{0, time.Minute, 8*time.Minute + 59*time.Second,
		time.Second, time.Minute, 9 * time.Minute}
	for i, step := range steps {
		clock.advance(step)
		if got := readSize(t, m).SizeFresh; got != want[i] {
			t.Fatalf("step %d: fresh %v, want %v", i, got, want[i])
		}
	}
	if r.measured() != 3 {
		t.Fatalf("%d measurements, want 3", r.measured())
	}
}

// Concurrency: measurements without a share, in parallel, all get
// distinct sequences (the -race run is an owner action: no cgo here).
func TestSizeSeq_ConcurrentMeasurementsGetDistinctSequences(t *testing.T) {
	var none *SizeShare
	c := &sizeCounter{value: 1}
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[uint64]bool{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := none.Measure(context.Background(), "", 0, c.measure)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || got.seq == 0 || seen[got.seq] {
				t.Errorf("sequence %d (err %v) zero or repeated", got.seq, err)
			}
			seen[got.seq] = true
		}()
	}
	wg.Wait()
	if len(seen) != 8 {
		t.Fatalf("%d distinct sequences from 8 measurements", len(seen))
	}
}

// Invalid input: not applicable; the sequence is internal and takes no input.
