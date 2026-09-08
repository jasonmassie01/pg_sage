package providerobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/verify"
)

type fakeAPI struct {
	mu      sync.Mutex
	now     time.Time
	idle    float64
	failed  bool
	entries []logwatch.LogEntry
}

func (f *fakeAPI) Metrics(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed {
		return "", errors.New("provider down")
	}
	return scrape(f.now, f.idle), nil
}

func (f *fakeAPI) Logs(context.Context, string, time.Time, time.Time) ([]logwatch.LogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed {
		return nil, errors.New("provider down")
	}
	return f.entries, nil
}

func TestRuntimePollDerivesTelemetryAndDeduplicatesLogs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeAPI{now: now, idle: 100, entries: []logwatch.LogEntry{
		{Database: "postgres", Timestamp: now.Add(-2 * time.Minute), Message: "deadlock detected"}}}
	delivered := 0
	r, err := NewRuntime(api, "postgres", func(_ context.Context, entries []logwatch.LogEntry) error {
		delivered += len(entries)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Poll(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	api.now = now.Add(time.Minute)
	api.idle = 145
	if err := r.Poll(context.Background(), api.now); err != nil {
		t.Fatal(err)
	}
	got := r.Telemetry()
	if got.CPUPct == nil || *got.CPUPct != 25 || delivered != 1 {
		t.Fatalf("CPU %#v deliveries %d", got, delivered)
	}
	_, err = r.CurrentLoad(context.Background())
	if !errors.Is(err, verify.ErrLoadTelemetryUnavailable) {
		t.Fatalf("missing I/O admitted: %v", err)
	}
	*got.CPUPct = 90
	if *r.Telemetry().CPUPct != 25 {
		t.Fatal("telemetry snapshot aliases mutable cache")
	}
}

func TestRuntimeFailureInvalidatesCacheAndRetriesDelivery(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	api := &fakeAPI{now: now, idle: 100,
		entries: []logwatch.LogEntry{{Timestamp: now.Add(-time.Minute)}}}
	attempts := 0
	r, _ := NewRuntime(api, "postgres", func(context.Context, []logwatch.LogEntry) error {
		attempts++
		if attempts == 1 {
			return errors.New("store down")
		}
		return nil
	})
	if err := r.Poll(context.Background(), now); err == nil {
		t.Fatal("swallowed sink error")
	}
	api.now = now.Add(time.Minute)
	api.idle = 145
	if err := r.Poll(context.Background(), api.now); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal("failed delivery was lost")
	}
	api.failed = true
	if err := r.Poll(context.Background(), api.now); err == nil {
		t.Fatal("swallowed API outage")
	}
	if r.Telemetry().CPUPct != nil {
		t.Fatal("outage left old CPU evidence available")
	}
}

func TestRuntimeLifecycleAndConcurrentReaders(t *testing.T) {
	if _, err := NewRuntime(nil, "postgres", nil); err == nil {
		t.Fatal("accepted nil API")
	}
	api := &fakeAPI{now: time.Now(), idle: 100}
	r, _ := NewRuntime(api, "postgres", nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Telemetry(); _, _ = r.CurrentLoad(context.Background()) }()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx, nil)
	wg.Wait()
	if err := r.Poll(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
	if !r.Telemetry().ObservedAt.IsZero() {
		t.Fatal("stopped runtime retained admission evidence")
	}
}
