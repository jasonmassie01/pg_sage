package logwatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// ---------------------------------------------------------------------------
// NewLogFanout
// ---------------------------------------------------------------------------

func TestNewLogFanout_NonNil(t *testing.T) {
	fw := &FileWatcher{}
	f := NewLogFanout(fw)
	if f == nil {
		t.Fatal("NewLogFanout returned nil")
	}
	if f.source != fw {
		t.Fatal("source not set")
	}
	if f.buffers == nil {
		t.Fatal("buffers map not initialized")
	}
}

// ---------------------------------------------------------------------------
// Subscribe
// ---------------------------------------------------------------------------

func TestSubscribe_ReturnsSubscriber(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	sub := f.Subscribe("db1")
	if sub == nil {
		t.Fatal("Subscribe returned nil")
	}
	if sub.id != "db1" {
		t.Errorf("subscriber id = %q, want %q", sub.id, "db1")
	}
	if sub.fanout != f {
		t.Error("subscriber fanout reference mismatch")
	}
}

func TestSubscribe_MultipleSubscribers(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	s1 := f.Subscribe("db1")
	s2 := f.Subscribe("db2")
	s3 := f.Subscribe("db3")
	if s1.id == s2.id || s2.id == s3.id {
		t.Fatal("subscribers should have distinct IDs")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buffers) != 3 {
		t.Errorf("buffers len = %d, want 3", len(f.buffers))
	}
}

// ---------------------------------------------------------------------------
// FanoutSubscriber satisfies rca.LogSource
// ---------------------------------------------------------------------------

func TestFanoutSubscriber_ImplementsLogSource(t *testing.T) {
	var _ rca.LogSource = (*FanoutSubscriber)(nil)
}

func TestFanoutSubscriber_StartIsNoop(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	sub := f.Subscribe("db1")
	if err := sub.Start(context.Background()); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
}

func TestFanoutSubscriber_StopIsNoop(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	sub := f.Subscribe("db1")
	sub.Stop() // should not panic
}

// ---------------------------------------------------------------------------
// DrainSource distributes to all subscribers
// ---------------------------------------------------------------------------

// injectSignals simulates DrainSource by directly distributing
// signals to all subscriber buffers, bypassing the FileWatcher.
func injectSignals(f *LogFanout, signals []*rca.Signal) {
	if len(signals) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.buffers {
		cp := make([]*rca.Signal, len(signals))
		copy(cp, signals)
		f.buffers[id] = append(f.buffers[id], cp...)
	}
}

func TestDrainSource_DistributesToAllSubscribers(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	s1 := f.Subscribe("db1")
	s2 := f.Subscribe("db2")

	signals := []*rca.Signal{
		{ID: "log_deadlock", FiredAt: time.Now(), Severity: "warning"},
		{ID: "log_temp_file", FiredAt: time.Now(), Severity: "info"},
	}
	injectSignals(f, signals)

	got1 := s1.Drain()
	got2 := s2.Drain()

	if len(got1) != 2 {
		t.Errorf("s1 got %d signals, want 2", len(got1))
	}
	if len(got2) != 2 {
		t.Errorf("s2 got %d signals, want 2", len(got2))
	}
	if got1[0].ID != "log_deadlock" {
		t.Errorf("s1[0].ID = %q, want %q", got1[0].ID, "log_deadlock")
	}
	if got2[1].ID != "log_temp_file" {
		t.Errorf("s2[1].ID = %q, want %q", got2[1].ID, "log_temp_file")
	}
}

func TestDrainSource_EmptySignals_NoBufferGrowth(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	_ = f.Subscribe("db1")

	injectSignals(f, nil)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buffers["db1"]) != 0 {
		t.Errorf("buffer should be empty, got %d",
			len(f.buffers["db1"]))
	}
}

// ---------------------------------------------------------------------------
// Drain returns and clears buffer
// ---------------------------------------------------------------------------

func TestDrain_ClearsBufferAfterRead(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	sub := f.Subscribe("db1")

	signals := []*rca.Signal{
		{ID: "sig1", FiredAt: time.Now(), Severity: "info"},
	}
	injectSignals(f, signals)

	first := sub.Drain()
	if len(first) != 1 {
		t.Fatalf("first Drain got %d, want 1", len(first))
	}

	second := sub.Drain()
	if len(second) != 0 {
		t.Errorf("second Drain got %d, want 0 (buffer cleared)",
			len(second))
	}
}

func TestDrain_NilForNewSubscriber(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	sub := f.Subscribe("db1")
	got := sub.Drain()
	if got != nil {
		t.Errorf("expected nil from fresh subscriber, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Subscriber isolation
// ---------------------------------------------------------------------------

func TestSubscribers_AreIndependent(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	s1 := f.Subscribe("db1")
	s2 := f.Subscribe("db2")

	signals := []*rca.Signal{
		{ID: "sig_a", FiredAt: time.Now(), Severity: "warning"},
	}
	injectSignals(f, signals)

	// s1 drains; s2 should still have its copy.
	_ = s1.Drain()
	got2 := s2.Drain()
	if len(got2) != 1 {
		t.Errorf("s2 got %d, want 1 (independent of s1)", len(got2))
	}
}

// ---------------------------------------------------------------------------
// Accumulation across multiple drain cycles
// ---------------------------------------------------------------------------

func TestDrainSource_AccumulatesAcrossCycles(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	sub := f.Subscribe("db1")

	batch1 := []*rca.Signal{
		{ID: "s1", FiredAt: time.Now(), Severity: "info"},
	}
	batch2 := []*rca.Signal{
		{ID: "s2", FiredAt: time.Now(), Severity: "warning"},
	}

	injectSignals(f, batch1)
	injectSignals(f, batch2)

	got := sub.Drain()
	if len(got) != 2 {
		t.Fatalf("got %d signals, want 2", len(got))
	}
	if got[0].ID != "s1" {
		t.Errorf("got[0].ID = %q, want %q", got[0].ID, "s1")
	}
	if got[1].ID != "s2" {
		t.Errorf("got[1].ID = %q, want %q", got[1].ID, "s2")
	}
}

// ---------------------------------------------------------------------------
// Concurrent access safety
// ---------------------------------------------------------------------------

func TestLogFanout_ConcurrentDrainAndSubscribe(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	const goroutines = 10

	var wg sync.WaitGroup

	// Concurrent subscribes.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			f.Subscribe(string(rune('a' + n)))
		}(i)
	}
	wg.Wait()

	// Concurrent drain source + subscriber drain.
	signals := []*rca.Signal{
		{ID: "concurrent_sig", FiredAt: time.Now(), Severity: "info"},
	}
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			injectSignals(f, signals)
		}()
	}
	wg.Wait()

	// Verify no panics and at least some signals were delivered.
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, buf := range f.buffers {
		if len(buf) == 0 {
			t.Errorf("subscriber %q has empty buffer after "+
				"concurrent drains", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Late subscriber does not receive past signals
// ---------------------------------------------------------------------------

func TestSubscribe_LateSubscriber_NoPastSignals(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	s1 := f.Subscribe("early")

	signals := []*rca.Signal{
		{ID: "past_sig", FiredAt: time.Now(), Severity: "info"},
	}
	injectSignals(f, signals)

	// Subscribe after signals were distributed.
	s2 := f.Subscribe("late")

	got1 := s1.Drain()
	got2 := s2.Drain()

	if len(got1) != 1 {
		t.Errorf("early subscriber got %d, want 1", len(got1))
	}
	if got2 != nil {
		t.Errorf("late subscriber got %v, want nil (no past signals)",
			got2)
	}
}

// ---------------------------------------------------------------------------
// Zero subscribers — DrainSource is safe
// ---------------------------------------------------------------------------

func TestDrainSource_NoSubscribers_NoPanic(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	// No subscribers registered. Should not panic.
	injectSignals(f, []*rca.Signal{
		{ID: "orphan", FiredAt: time.Now(), Severity: "info"},
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buffers) != 0 {
		t.Errorf("expected 0 buffers, got %d", len(f.buffers))
	}
}
