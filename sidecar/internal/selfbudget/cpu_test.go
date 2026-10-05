package selfbudget

import (
	"sync"
	"testing"
	"time"
)

// fakeCPU is a scripted process-CPU reader and clock.
type fakeCPU struct {
	mu  sync.Mutex
	cpu time.Duration
	ok  bool
	at  time.Time
}

func (f *fakeCPU) read() (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cpu, f.ok
}

func (f *fakeCPU) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func (f *fakeCPU) advance(wall, cpu time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at = f.at.Add(wall)
	f.cpu += cpu
}

func newFake() *fakeCPU {
	return &fakeCPU{ok: true, at: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		cpu: 10 * time.Second}
}

func TestCPUMeter_FirstObservationIsUnknown(t *testing.T) {
	f := newFake()
	m := NewCPUMeter(f.read, f.now)
	if v, ok := m.Observe(time.Minute); ok || v != 0 {
		t.Fatalf("first observation = %v, %v; want unknown", v, ok)
	}
}

// 1.5 s of CPU over 30 s of wall time is 3 s per 60 s collector cycle.
func TestCPUMeter_RatePerCycle(t *testing.T) {
	f := newFake()
	m := NewCPUMeter(f.read, f.now)
	m.Observe(time.Minute)
	f.advance(30*time.Second, 1500*time.Millisecond)
	v, ok := m.Observe(time.Minute)
	if !ok || v != 3000 {
		t.Fatalf("rate = %v, %v; want 3000 ms per cycle", v, ok)
	}
	f.advance(60*time.Second, 300*time.Millisecond)
	if v, ok := m.Observe(time.Minute); !ok || v != 300 {
		t.Fatalf("second window = %v, %v; want 300 (windows do not accumulate)", v, ok)
	}
	if v, ok := m.Last(); !ok || v != 300 {
		t.Fatalf("Last() = %v, %v; want the latest window", v, ok)
	}
}

func TestCPUMeter_UnreadableCPUIsUnknownAndResets(t *testing.T) {
	f := newFake()
	m := NewCPUMeter(f.read, f.now)
	m.Observe(time.Minute)
	f.ok = false
	f.advance(time.Minute, time.Second)
	if _, ok := m.Observe(time.Minute); ok {
		t.Fatal("unreadable CPU must be unknown")
	}
	f.ok = true
	f.advance(time.Minute, time.Second)
	if _, ok := m.Observe(time.Minute); ok {
		t.Fatal("first reading after a failure has no window and must be unknown")
	}
	f.advance(time.Minute, 2*time.Second)
	if v, ok := m.Observe(time.Minute); !ok || v != 2000 {
		t.Fatalf("recovered rate = %v, %v; want 2000", v, ok)
	}
}

func TestCPUMeter_InvalidWindowsAreUnknown(t *testing.T) {
	cases := map[string]func(f *fakeCPU){
		"no wall time":     func(f *fakeCPU) { f.advance(0, time.Second) },
		"clock went back":  func(f *fakeCPU) { f.advance(-time.Minute, time.Second) },
		"cpu went back":    func(f *fakeCPU) { f.advance(time.Minute, -2*time.Second) },
		"cpu reader reset": func(f *fakeCPU) { f.cpu = 0; f.advance(time.Minute, 0) },
	}
	for name, step := range cases {
		f := newFake()
		m := NewCPUMeter(f.read, f.now)
		m.Observe(time.Minute)
		step(f)
		if v, ok := m.Observe(time.Minute); ok {
			t.Errorf("%s: rate = %v, want unknown", name, v)
		}
	}
	f := newFake()
	m := NewCPUMeter(f.read, f.now)
	m.Observe(time.Minute)
	f.advance(time.Minute, time.Second)
	if _, ok := m.Observe(0); ok {
		t.Error("zero cycle must be unknown")
	}
}

func TestCPUMeter_NilIsSafe(t *testing.T) {
	var m *CPUMeter
	if v, ok := m.Observe(time.Minute); ok || v != 0 {
		t.Fatal("nil meter must be unknown")
	}
	if _, ok := m.Last(); ok {
		t.Fatal("nil meter Last must be unknown")
	}
	if _, ok := NewCPUMeter(nil, nil).Observe(time.Minute); ok {
		t.Fatal("a meter without a reader must be unknown")
	}
}

func TestCPUMeter_ConcurrentObserve(t *testing.T) {
	f := newFake()
	m := NewCPUMeter(f.read, f.now)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				f.advance(time.Second, 10*time.Millisecond)
				if v, ok := m.Observe(time.Minute); ok && (v < 0 || v > 60_000) {
					t.Errorf("impossible rate %v", v)
				}
				m.Last()
			}
		}()
	}
	wg.Wait()
}

// The real reader: CPU time is readable on the platforms pg_sage ships on
// and grows when the process works.
func TestProcessCPU_GrowsWithWork(t *testing.T) {
	before, ok := ProcessCPU()
	if !ok {
		t.Skip("process CPU time is not readable on this platform")
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	x := 0
	for time.Now().Before(deadline) {
		x++
	}
	after, ok := ProcessCPU()
	if !ok || after <= before {
		t.Fatalf("CPU %s -> %s (ok %v) after busy work (%d)", before, after, ok, x)
	}
}
