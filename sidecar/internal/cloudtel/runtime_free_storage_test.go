package cloudtel

import (
	"context"
	"errors"
	"testing"
)

// guard-01a: FreeStorage is check_pressure's free storage input
// (pressure.StorageSource): fresh effective free bytes, the capacity and
// when free storage was sampled; unavailable when unknown, stale or
// unbounded. No concurrency test: it only reads the sample under the
// runtime's RWMutex, which the other readers already exercise.

func TestFreeStorageIsTheFreshEffectiveSample(t *testing.T) {
	c := &clock{t: guardNow}
	s := healthySample(guardNow)
	r := newTestRuntime(t, &stubSource{provider: "rds", samples: []Sample{s}}, c)
	if _, _, _, err := r.FreeStorage(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("before the first poll err = %v, want ErrUnavailable", err)
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	wantFree, wantCap, ok := s.EffectiveFreeStorage(guardNow)
	if !ok {
		t.Fatal("the healthy sample has no effective free storage")
	}
	free, capacity, at, err := r.FreeStorage(context.Background())
	if err != nil || free != wantFree || capacity != wantCap ||
		!at.Equal(s.FreeStorageBytes.At) {
		t.Fatalf("FreeStorage = %v, %v, %v, %v; want %v, %v at %v", free, capacity, at, err,
			wantFree, wantCap, s.FreeStorageBytes.At)
	}
	c.advance(MaxPointAge + 1)
	if _, _, _, err := r.FreeStorage(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale sample err = %v, want ErrUnavailable", err)
	}
}

func TestFreeStorageUnboundedAndCancelled(t *testing.T) {
	c := &clock{t: guardNow}
	s := healthySample(guardNow)
	s.StorageAutoGrows = true
	r := newTestRuntime(t, &stubSource{provider: "aurora", samples: []Sample{s}}, c)
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if _, _, _, err := r.FreeStorage(context.Background()); !errors.Is(err, ErrUnbounded) {
		t.Fatalf("auto-growing storage err = %v, want ErrUnbounded", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := r.FreeStorage(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v, want context.Canceled", err)
	}
	if _, _, _, err := Unavailable("orders", "rds", "no credentials").FreeStorage(
		context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable runtime err = %v, want ErrUnavailable", err)
	}
}
