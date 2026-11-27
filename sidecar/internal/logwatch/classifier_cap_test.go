package logwatch

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// rca-06: the per-drain classifier cap applies only after the critical
// check, and every line it skips is counted.

func capEntry(i int, state, level, msg string) LogEntry {
	return baseEntry(func(e *LogEntry) {
		e.Timestamp = time.Date(2026, 11, 8, 12, 0, 0, i, time.UTC)
		e.SQLState = state
		e.ErrorLevel = level
		e.PID = 5000 + i
		e.Database = fmt.Sprintf("cap_db_%d", i)
		e.Message = msg
	})
}

// Boundary: a cap of 100 followed by a 53100 line still classifies it.
func TestClassifierCapStillClassifiesDiskFull(t *testing.T) {
	c := NewClassifier(ClassifierConfig{MaxLinesPerCycle: 100}, nopLog)
	for i := 0; i < 150; i++ {
		c.Classify(capEntry(i, "42P01", "ERROR", "relation does not exist"))
	}
	sig := c.Classify(capEntry(150, "53100", "ERROR",
		"could not extend file: No space left on device"))
	if sig == nil || sig.ID != "log_disk_full" {
		t.Fatalf("disk-full after the cap = %+v, want log_disk_full", sig)
	}
	if sig.Severity != "critical" {
		t.Fatalf("severity = %q, want critical", sig.Severity)
	}
	if capped := c.ResetCycle(); capped != 50 {
		t.Fatalf("capped lines = %d, want 50", capped)
	}
}

// A non-critical pattern line past the cap is still skipped and counted.
func TestClassifierCapSkipsNonCriticalSignals(t *testing.T) {
	c := NewClassifier(ClassifierConfig{MaxLinesPerCycle: 2}, nopLog)
	c.Classify(capEntry(0, "42P01", "ERROR", "noise"))
	c.Classify(capEntry(1, "42P01", "ERROR", "noise"))
	if sig := c.Classify(capEntry(2, "55P03", "ERROR",
		"canceling statement due to lock timeout")); sig != nil {
		t.Fatalf("lock timeout past the cap = %+v, want nil", sig)
	}
	if capped := c.ResetCycle(); capped != 1 {
		t.Fatalf("capped = %d, want 1", capped)
	}
	// After the reset the cap is fresh again and the count restarts.
	if sig := c.Classify(capEntry(3, "55P03", "ERROR",
		"canceling statement due to lock timeout")); sig == nil {
		t.Fatal("lock timeout after ResetCycle was dropped")
	}
	if capped := c.ResetCycle(); capped != 0 {
		t.Fatalf("capped after a quiet cycle = %d, want 0", capped)
	}
}

// PANIC and FATAL bypass the cap even without a critical SQLSTATE.
func TestClassifierCapBypassedByPanicAndFatal(t *testing.T) {
	c := NewClassifier(ClassifierConfig{MaxLinesPerCycle: 1}, nopLog)
	c.Classify(capEntry(0, "42P01", "ERROR", "noise"))
	if sig := c.Classify(capEntry(1, "XX000", "PANIC",
		"could not write to file")); sig == nil ||
		sig.ID != "log_panic_server_crash" {
		t.Fatalf("PANIC past the cap = %+v, want log_panic_server_crash", sig)
	}
	if sig := c.Classify(capEntry(2, "53300", "FATAL",
		"sorry, too many clients already")); sig == nil ||
		sig.ID != "log_connection_refused" {
		t.Fatalf("FATAL past the cap = %+v, want log_connection_refused", sig)
	}
	if capped := c.ResetCycle(); capped != 0 {
		t.Fatalf("critical lines counted as capped: %d", capped)
	}
}

// Zero cap means unlimited: nothing is ever counted as capped.
func TestClassifierCapZeroIsUnlimited(t *testing.T) {
	c := NewClassifier(ClassifierConfig{}, nopLog)
	for i := 0; i < 500; i++ {
		c.Classify(capEntry(i, "42P01", "ERROR", "noise"))
	}
	if sig := c.Classify(capEntry(500, "55P03", "ERROR",
		"canceling statement due to lock timeout")); sig == nil {
		t.Fatal("unlimited classifier dropped a signal")
	}
	if capped := c.ResetCycle(); capped != 0 {
		t.Fatalf("capped = %d with no cap", capped)
	}
}

// Concurrent Classify calls count every capped line exactly once.
func TestClassifierCapConcurrentCount(t *testing.T) {
	c := NewClassifier(ClassifierConfig{MaxLinesPerCycle: 10}, nopLog)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				c.Classify(capEntry(g*100+i, "42P01", "ERROR", "noise"))
			}
		}(g)
	}
	wg.Wait()
	if capped := c.ResetCycle(); capped != 190 {
		t.Fatalf("capped = %d, want 190", capped)
	}
}
