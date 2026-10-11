package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/pgaudit"
)

type fakeDrainer struct {
	mu      sync.Mutex
	batches [][]logwatch.LogEntry
	stopped bool
}

func (f *fakeDrainer) Drain() []logwatch.LogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return nil
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b
}

func (f *fakeDrainer) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}

type fakeIngester struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (f *fakeIngester) Ingest(_ context.Context, e []logwatch.LogEntry) (pgaudit.Stats,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		f.fail = false
		return pgaudit.Stats{}, errors.New("db down")
	}
	return pgaudit.Stats{Stored: len(e)}, nil
}

// The loop ingests each non-empty drain, survives an ingest failure, and
// unsubscribes when the runtime stops.
func TestRunPGAuditCorrelation(t *testing.T) {
	entries := []logwatch.LogEntry{{Message: "AUDIT: x"}}
	d := &fakeDrainer{batches: [][]logwatch.LogEntry{entries, nil, entries, entries}}
	ing := &fakeIngester{fail: true}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runPGAuditCorrelation(ctx, d, ing, 5*time.Millisecond, "test")
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ing.mu.Lock()
		calls := ing.calls
		ing.mu.Unlock()
		if calls >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if ing.calls != 3 {
		t.Fatalf("ingest calls = %d, want 3 (empty drains skipped, failure survived)",
			ing.calls)
	}
	if !d.stopped {
		t.Fatalf("subscription not stopped")
	}
}
