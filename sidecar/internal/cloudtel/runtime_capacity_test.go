package cloudtel

import (
	"context"
	"errors"
	"testing"
)

// clouds-07: CapacityBytes tells storage that grows without a bound
// (Aurora, Cloud SQL auto-resize without a limit) apart from capacity that
// is merely unknown, so the WAL custodian can record capacity_unbounded.
// No concurrency test: CapacityBytes only reads the sample under the
// runtime's RWMutex, which the other readers already exercise.

func TestCapacityBytesUnboundedStorageIsDistinguished(t *testing.T) {
	c := &clock{t: guardNow}
	s := healthySample(guardNow)
	s.StorageAutoGrows, s.StorageCapacityBytes = true, nil
	r := newTestRuntime(t, &stubSource{provider: "aurora", samples: []Sample{s}}, c)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	got, err := r.CapacityBytes(context.Background())
	if !errors.Is(err, ErrUnbounded) || !errors.Is(err, ErrUnavailable) || got != 0 {
		t.Fatalf("auto-growing storage CapacityBytes = %d, %v; want 0, ErrUnbounded "+
			"wrapping ErrUnavailable", got, err)
	}
}

func TestCapacityBytesUnknownIsNotUnbounded(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds",
		samples: []Sample{healthySample(guardNow)}}, c)
	if _, err := r.CapacityBytes(ctx); !errors.Is(err, ErrUnavailable) ||
		errors.Is(err, ErrUnbounded) {
		t.Fatalf("before the first poll err = %v; want unavailable, not unbounded", err)
	}
	if err := r.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	c.advance(MaxPointAge + 1)
	if got, err := r.CapacityBytes(ctx); !errors.Is(err, ErrUnavailable) ||
		errors.Is(err, ErrUnbounded) || got != 0 {
		t.Fatalf("stale capacity = %d, %v; want 0, unavailable (not unbounded)", got, err)
	}
	if _, err := Unavailable("orders", "rds", "no credentials").CapacityBytes(ctx); errors.Is(
		err, ErrUnbounded) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable runtime err = %v", err)
	}
}

func TestCapacityBytesZeroCapacityIsUnknown(t *testing.T) {
	c := &clock{t: guardNow}
	s := healthySample(guardNow)
	s.StorageCapacityBytes = &Point{Value: 0, At: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds", samples: []Sample{s}}, c)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got, err := r.CapacityBytes(context.Background()); err == nil ||
		errors.Is(err, ErrUnbounded) || got != 0 {
		t.Fatalf("zero capacity = %d, %v; want unknown", got, err)
	}
}

func TestCapacityBytesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &clock{t: guardNow}
	r := newTestRuntime(t, &stubSource{provider: "rds",
		samples: []Sample{healthySample(guardNow)}}, c)
	if _, err := r.CapacityBytes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
}
