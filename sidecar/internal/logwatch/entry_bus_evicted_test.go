package logwatch

import (
	"fmt"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// audit-06: a full parsed-entry subscription counts what it evicts, per
// subscriber, and TakeEvicted hands each eviction out once.
func TestEntrySubscriberCountsEvictions(t *testing.T) {
	fw := NewFileWatcher(config.LogWatchConfig{}, wave4NopLog)
	slow := fw.SubscribeEntries("slow", "")
	other := fw.SubscribeEntries("other", "db-other")
	// Boundary: filling the buffer exactly evicts nothing.
	for i := 0; i < maxBufferSize; i++ {
		fw.publishEntry(LogEntry{Database: fmt.Sprintf("db-%d", i)})
	}
	if n := slow.TakeEvicted(); n != 0 {
		t.Fatalf("a buffer filled to its cap evicted %d entries", n)
	}
	for i := 0; i < 7; i++ {
		fw.publishEntry(LogEntry{Database: "db-late"})
	}
	if n := slow.TakeEvicted(); n != 7 {
		t.Fatalf("evicted = %d, want 7", n)
	}
	if n := slow.TakeEvicted(); n != 0 {
		t.Fatalf("evictions handed out twice (%d)", n)
	}
	if n := other.TakeEvicted(); n != 0 {
		t.Fatalf("a subscriber that received nothing counted %d evictions", n)
	}
	if got := slow.Drain(); len(got) != maxBufferSize || got[0].Database != "db-7" {
		t.Fatalf("drained %d entries from %q, want %d from db-7", len(got),
			got[0].Database, maxBufferSize)
	}
}

// Nil/empty: a nil or stopped subscriber reports no evictions.
func TestEntrySubscriberTakeEvictedNilAndStopped(t *testing.T) {
	var nilSub *EntrySubscriber
	if n := nilSub.TakeEvicted(); n != 0 {
		t.Fatalf("nil subscriber evicted = %d", n)
	}
	fw := NewFileWatcher(config.LogWatchConfig{}, wave4NopLog)
	sub := fw.SubscribeEntries("gone", "")
	for i := 0; i < maxBufferSize+2; i++ {
		fw.publishEntry(LogEntry{})
	}
	sub.Stop()
	if n := sub.TakeEvicted(); n != 0 {
		t.Fatalf("stopped subscriber evicted = %d", n)
	}
}

// Concurrent access: publishing and taking at once loses no eviction.
func TestEntrySubscriberEvictionsConcurrent(t *testing.T) {
	fw := NewFileWatcher(config.LogWatchConfig{}, wave4NopLog)
	sub := fw.SubscribeEntries("busy", "")
	for i := 0; i < maxBufferSize; i++ {
		fw.publishEntry(LogEntry{})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	taken := 0
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				fw.publishEntry(LogEntry{})
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				n := sub.TakeEvicted()
				mu.Lock()
				taken += n
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	taken += sub.TakeEvicted()
	if taken != 200 {
		t.Fatalf("evictions taken = %d, want 200", taken)
	}
}
