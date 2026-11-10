package providerobs

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rejectingAPI serves fresh metrics and a fixed rejected-row count per read.
type rejectingAPI struct {
	mu       sync.Mutex
	now      time.Time
	rejected int
}

func (a *rejectingAPI) Metrics(context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return scrape(a.now, 100), nil
}

func (a *rejectingAPI) Logs(context.Context, string, time.Time, time.Time) (
	[]logwatch.LogEntry, int, error,
) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return nil, a.rejected, nil
}

func float(v float64) *float64 { return &v }

func TestTelemetryHostMemory(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		t                Telemetry
		total, available int64
	}{
		{"fresh", Telemetry{ObservedAt: now, MemoryTotalBytes: float(1000),
			MemoryAvailableBytes: float(250)}, 1000, 250},
		{"exactly max age", Telemetry{ObservedAt: now.Add(-MaxMetricAge),
			MemoryTotalBytes: float(1000), MemoryAvailableBytes: float(250)}, 1000, 250},
		{"just stale", Telemetry{ObservedAt: now.Add(-MaxMetricAge - time.Nanosecond),
			MemoryTotalBytes: float(1000), MemoryAvailableBytes: float(250)}, 0, 0},
		{"older than two minutes", Telemetry{ObservedAt: now.Add(-3 * time.Minute),
			MemoryTotalBytes: float(1000)}, 0, 0},
		{"future", Telemetry{ObservedAt: now.Add(6 * time.Second),
			MemoryTotalBytes: float(1000)}, 0, 0},
		{"zero value", Telemetry{}, 0, 0},
		{"no total", Telemetry{ObservedAt: now, MemoryAvailableBytes: float(250)}, 0, 0},
		{"zero total", Telemetry{ObservedAt: now, MemoryTotalBytes: float(0),
			MemoryAvailableBytes: float(0)}, 0, 0},
		{"NaN total", Telemetry{ObservedAt: now, MemoryTotalBytes: float(math.NaN())}, 0, 0},
		{"infinite total", Telemetry{ObservedAt: now, MemoryTotalBytes: float(math.Inf(1))},
			0, 0},
		{"available above total", Telemetry{ObservedAt: now, MemoryTotalBytes: float(1000),
			MemoryAvailableBytes: float(1001)}, 1000, 0},
		{"available equals total", Telemetry{ObservedAt: now, MemoryTotalBytes: float(1000),
			MemoryAvailableBytes: float(1000)}, 1000, 1000},
		{"NaN available", Telemetry{ObservedAt: now, MemoryTotalBytes: float(1000),
			MemoryAvailableBytes: float(math.NaN())}, 1000, 0},
		{"no available", Telemetry{ObservedAt: now, MemoryTotalBytes: float(1000)}, 1000, 0},
	}
	for _, tc := range cases {
		total, available := tc.t.HostMemory(now)
		if total != tc.total || available != tc.available {
			t.Errorf("%s: HostMemory = (%d, %d), want (%d, %d)", tc.name, total, available,
				tc.total, tc.available)
		}
	}
}

func TestRuntimeHostMemoryFollowsPollsAndInvalidation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeAPI{now: now, idle: 100}
	r, err := NewRuntime(api, "postgres", nil)
	if err != nil {
		t.Fatal(err)
	}
	if total, available := r.HostMemory(); total != 0 || available != 0 {
		t.Fatalf("memory before any poll = (%d, %d)", total, available)
	}
	// One scrape is enough for memory; CPU needs two.
	if err := r.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if total, available := r.HostMemory(); total != 1000 || available != 250 {
		t.Fatalf("memory after a poll = (%d, %d), want (1000, 250)", total, available)
	}
	api.failed = true
	if err := r.Poll(context.Background(), now); err == nil {
		t.Fatal("swallowed outage")
	}
	if total, available := r.HostMemory(); total != 0 || available != 0 {
		t.Fatalf("outage left memory evidence (%d, %d)", total, available)
	}
}

func TestRuntimeRejectionCounterAggregatesPerPoll(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 3, 30, 0, time.UTC)
	api := &rejectingAPI{now: now, rejected: 3}
	r, _ := NewRuntime(api, "postgres", func(context.Context, []logwatch.LogEntry) error {
		return nil
	})
	type report struct {
		rejected int
		total    uint64
	}
	var reports []report
	r.SetRejectionReporter(func(rejected int, total uint64) {
		reports = append(reports, report{rejected, total})
	})
	if err := r.pollLogs(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	first := r.through
	api.rejected = 0
	if err := r.pollLogs(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	api.rejected = 2
	if err := r.pollLogs(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := []report{{3, 3}, {2, 5}}
	if len(reports) != len(want) || reports[0] != want[0] || reports[1] != want[1] {
		t.Fatalf("reports = %v, want %v (none for a clean poll)", reports, want)
	}
	if r.RejectedLogRecords() != 5 || !r.through.After(first) {
		t.Fatalf("total = %d through %v -> %v", r.RejectedLogRecords(), first, r.through)
	}
}

func TestRuntimeRejectionWithoutReporter(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 3, 30, 0, time.UTC)
	r, _ := NewRuntime(&rejectingAPI{now: now, rejected: 1}, "postgres",
		func(context.Context, []logwatch.LogEntry) error { return nil })
	if err := r.pollLogs(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if r.RejectedLogRecords() != 1 {
		t.Fatalf("total = %d, want 1 without a reporter", r.RejectedLogRecords())
	}
}

func TestRuntimeHostMemoryConcurrentReaders(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeAPI{now: now, idle: 100}
	r, _ := NewRuntime(api, "postgres", nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				total, available := r.HostMemory()
				if total != 0 && (total != 1000 || available != 250) {
					t.Errorf("torn read (%d, %d)", total, available)
					return
				}
				_ = r.RejectedLogRecords()
			}
		}()
	}
	for range 20 {
		_ = r.Poll(context.Background(), now)
	}
	wg.Wait()
}
