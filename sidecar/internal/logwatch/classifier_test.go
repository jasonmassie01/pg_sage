package logwatch

import (
	"testing"
	"time"
)

// nopLog is a no-op logger for classifier tests.
func nopLog(string, string, ...any) {}

// ---------------------------------------------------------------------------
// NewClassifier
// ---------------------------------------------------------------------------

func TestNewClassifier_DefaultDedupWindow(t *testing.T) {
	c := NewClassifier(ClassifierConfig{DedupWindowS: 0}, nopLog)
	if c.dedupWindow != 60*time.Second {
		t.Fatalf("dedupWindow = %v, want 60s", c.dedupWindow)
	}
}

func TestNewClassifier_CustomDedupWindow(t *testing.T) {
	c := NewClassifier(ClassifierConfig{DedupWindowS: 120}, nopLog)
	if c.dedupWindow != 120*time.Second {
		t.Fatalf("dedupWindow = %v, want 120s", c.dedupWindow)
	}
}

func TestNewClassifier_ExcludeApps(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		ExcludeApps: []string{"pg_sage", "pg_dump"},
	}, nopLog)
	if !c.excludeApps["pg_sage"] {
		t.Error("pg_sage should be excluded")
	}
	if !c.excludeApps["pg_dump"] {
		t.Error("pg_dump should be excluded")
	}
	if c.excludeApps["psql"] {
		t.Error("psql should not be excluded")
	}
}

// ---------------------------------------------------------------------------
// Helper: build entries for classification
// ---------------------------------------------------------------------------

func baseEntry(overrides func(*LogEntry)) LogEntry {
	e := LogEntry{
		Timestamp:   time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC),
		PID:         1000,
		Database:    "testdb",
		User:        "postgres",
		ErrorLevel:  "ERROR",
		Application: "psql",
	}
	if overrides != nil {
		overrides(&e)
	}
	return e
}

func defaultClassifier() *Classifier {
	return NewClassifier(ClassifierConfig{
		DedupWindowS:     60,
		TempFileMinBytes: 1024,
		MaxLinesPerCycle: 10000,
	}, nopLog)
}

// ---------------------------------------------------------------------------
// Classify — signal pattern matching
// ---------------------------------------------------------------------------

func TestClassify_Deadlock(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Message = "deadlock detected"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for deadlock, got nil")
	}
	if sig.ID != "log_deadlock_detected" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_deadlock_detected")
	}
	if sig.Severity != "critical" {
		t.Errorf("severity = %q, want %q", sig.Severity, "critical")
	}
}

func TestClassify_ConnectionRefused(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "53300"
		e.ErrorLevel = "FATAL"
		e.Message = "sorry, too many clients already"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for connection refused, got nil")
	}
	if sig.ID != "log_connection_refused" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_connection_refused")
	}
}

func TestClassify_OutOfMemory(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "53200"
		e.ErrorLevel = "ERROR"
		e.Message = "out of memory"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for OOM, got nil")
	}
	if sig.ID != "log_out_of_memory" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_out_of_memory")
	}
}

func TestClassify_DiskFull(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "53100"
		e.ErrorLevel = "ERROR"
		e.Message = "could not write to file: No space left on device"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for disk full, got nil")
	}
	if sig.ID != "log_disk_full" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_disk_full")
	}
}

func TestClassify_PanicServerCrash_ByMessage(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "server process was terminated by signal 11: Segmentation fault"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for panic/crash, got nil")
	}
	if sig.ID != "log_panic_server_crash" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_panic_server_crash")
	}
}

func TestClassify_PanicServerCrash_ByLevel(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "PANIC"
		e.Message = "something terrible"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for PANIC level, got nil")
	}
	if sig.ID != "log_panic_server_crash" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_panic_server_crash")
	}
}

func TestClassify_DataCorruption(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "XX001"
		e.ErrorLevel = "WARNING"
		e.Message = "page verification failed"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for data corruption, got nil")
	}
	if sig.ID != "log_data_corruption" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_data_corruption")
	}
}

func TestClassify_TxidWraparound(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "WARNING"
		e.Message = "database mydb must be vacuumed within 1000000 transactions"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for txid wraparound, got nil")
	}
	if sig.ID != "log_txid_wraparound_warning" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_txid_wraparound_warning")
	}
}

func TestClassify_ArchiveFailed(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "archive command failed with exit code 1"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for archive failed, got nil")
	}
	if sig.ID != "log_archive_failed" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_archive_failed")
	}
}

func TestClassify_TempFileCreated_AboveThreshold(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "temporary file: path /tmp/pg_sort.123, size 5000"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for temp file above threshold, got nil")
	}
	if sig.ID != "log_temp_file_created" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_temp_file_created")
	}
	sz, ok := sig.Metrics["temp_file_bytes"].(int64)
	if !ok || sz != 5000 {
		t.Errorf("temp_file_bytes = %v, want 5000", sig.Metrics["temp_file_bytes"])
	}
}

func TestClassify_TempFileCreated_BelowThreshold(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "temporary file: path /tmp/pg_sort.123, size 100"
	})
	sig := c.Classify(e)
	if sig != nil {
		t.Fatalf("expected nil for temp file below threshold, got %+v", sig)
	}
}

func TestClassify_CheckpointTooFrequent(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "checkpoints are occurring too frequently (5 seconds apart)"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for checkpoint too frequent, got nil")
	}
	if sig.ID != "log_checkpoint_too_frequent" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_checkpoint_too_frequent")
	}
}

func TestClassify_LockTimeout(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "55P03"
		e.ErrorLevel = "ERROR"
		e.Message = "canceling statement due to lock timeout"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for lock timeout, got nil")
	}
	if sig.ID != "log_lock_timeout" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_lock_timeout")
	}
}

func TestClassify_StatementTimeout(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.ErrorLevel = "ERROR"
		e.Message = "canceling statement due to statement timeout"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for statement timeout, got nil")
	}
	if sig.ID != "log_statement_timeout" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_statement_timeout")
	}
}

func TestClassify_ReplicationConflict(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "ERROR"
		e.Message = "canceling statement due to conflict with recovery"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for replication conflict, got nil")
	}
	if sig.ID != "log_replication_conflict" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_replication_conflict")
	}
}

func TestClassify_WALSegmentRemoved(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "ERROR"
		e.Message = "requested WAL segment has already been removed"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for WAL segment removed, got nil")
	}
	if sig.ID != "log_wal_segment_removed" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_wal_segment_removed")
	}
}

func TestClassify_AutovacuumCancel(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "canceling autovacuum task on table mydb.public.big_table"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for autovacuum cancel, got nil")
	}
	if sig.ID != "log_autovacuum_cancel" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_autovacuum_cancel")
	}
}

func TestClassify_ReplicationSlotInactive(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "WARNING"
		e.Message = "replication slot \"my_slot\" is inactive and will be dropped"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for replication slot inactive, got nil")
	}
	if sig.ID != "log_replication_slot_inactive" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_replication_slot_inactive")
	}
}

func TestClassify_AuthenticationFailure(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "28P01"
		e.ErrorLevel = "FATAL"
		e.Message = "password authentication failed for user \"baduser\""
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal for auth failure, got nil")
	}
	if sig.ID != "log_authentication_failure" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_authentication_failure")
	}
}

// ---------------------------------------------------------------------------
// Classify — slow query opt-in
// ---------------------------------------------------------------------------

func TestClassify_SlowQuery_OptInDisabled(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     60,
		SlowQueryEnabled: false,
		MaxLinesPerCycle: 10000,
	}, nopLog)
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "duration: 5000.123 ms  statement: SELECT pg_sleep(5)"
		e.Query = "SELECT pg_sleep(5)"
	})
	sig := c.Classify(e)
	if sig != nil {
		t.Fatalf("slow query should be suppressed when opt-in disabled, got %+v", sig)
	}
}

func TestClassify_SlowQuery_OptInEnabled(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     60,
		SlowQueryEnabled: true,
		MaxLinesPerCycle: 10000,
	}, nopLog)
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel = "LOG"
		e.Message = "duration: 5000.123 ms  statement: SELECT pg_sleep(5)"
		e.Query = "SELECT pg_sleep(5)"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected slow query signal when opt-in enabled, got nil")
	}
	if sig.ID != "log_slow_query" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_slow_query")
	}
	dur, ok := sig.Metrics["duration_ms"].(float64)
	if !ok || dur != 5000.123 {
		t.Errorf("duration_ms = %v, want 5000.123", sig.Metrics["duration_ms"])
	}
	q, ok := sig.Metrics["query"].(string)
	if !ok {
		t.Fatal("expected query in metrics")
	}
	if q != "SELECT pg_sleep(5)" {
		t.Errorf("query = %q, want %q", q, "SELECT pg_sleep(5)")
	}
}

// ---------------------------------------------------------------------------
// Classify — excluded apps
// ---------------------------------------------------------------------------

func TestClassify_ExcludedApp_Suppressed(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     60,
		ExcludeApps:      []string{"pg_sage"},
		MaxLinesPerCycle: 10000,
	}, nopLog)
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "57014"
		e.ErrorLevel = "ERROR"
		e.Application = "pg_sage"
		e.Message = "canceling statement due to statement timeout"
	})
	sig := c.Classify(e)
	if sig != nil {
		t.Fatalf("excluded app should suppress signal, got %+v", sig)
	}
}

func TestClassify_ExcludedApp_DeadlockNotSuppressed(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     60,
		ExcludeApps:      []string{"pg_sage"},
		MaxLinesPerCycle: 10000,
	}, nopLog)
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Application = "pg_sage"
		e.Message = "deadlock detected"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("deadlock from excluded app should NOT be suppressed")
	}
	if sig.ID != "log_deadlock_detected" {
		t.Errorf("signal ID = %q, want %q", sig.ID, "log_deadlock_detected")
	}
	si, ok := sig.Metrics["self_inflicted"].(bool)
	if !ok || !si {
		t.Error("expected self_inflicted=true in metrics")
	}
}

// ---------------------------------------------------------------------------
// Classify — level too low
// ---------------------------------------------------------------------------

func TestClassify_LevelTooLow(t *testing.T) {
	c := defaultClassifier()
	// Deadlock requires MinLevel ERROR. A LOG-level entry should not match.
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "40P01"
		e.ErrorLevel = "LOG"
		e.Message = "deadlock detected"
	})
	sig := c.Classify(e)
	if sig != nil {
		t.Fatalf("LOG-level entry should not fire ERROR-minimum signal, got %+v", sig)
	}
}

// ---------------------------------------------------------------------------
// Classify — signal metrics
// ---------------------------------------------------------------------------

func TestClassify_MetricsPopulated(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Message = "deadlock detected"
		e.Detail = "Process 1000 waits for ShareLock on transaction 12345"
		e.Application = "myapp"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("expected signal, got nil")
	}
	checks := map[string]any{
		"database":    "testdb",
		"user":        "postgres",
		"pid":         1000,
		"error_level": "ERROR",
		"sql_state":   "40P01",
		"application": "myapp",
	}
	for k, want := range checks {
		got, ok := sig.Metrics[k]
		if !ok {
			t.Errorf("missing metric %q", k)
			continue
		}
		if got != want {
			t.Errorf("metric %q = %v, want %v", k, got, want)
		}
	}
	// Detail should be present.
	if _, ok := sig.Metrics["detail"]; !ok {
		t.Error("expected detail in metrics")
	}
	// FiredAt should match the entry timestamp.
	if !sig.FiredAt.Equal(e.Timestamp) {
		t.Errorf("FiredAt = %v, want %v", sig.FiredAt, e.Timestamp)
	}
}

// ---------------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------------

func TestClassify_Dedup_SameSignalWithinWindow(t *testing.T) {
	c := defaultClassifier()
	ts := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

	e1 := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.PID = 1000
		e.Message = "deadlock detected"
	})
	e2 := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts.Add(10 * time.Second)
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.PID = 1001
		e.Message = "deadlock detected"
	})

	sig1 := c.Classify(e1)
	if sig1 == nil {
		t.Fatal("first occurrence should produce a signal")
	}
	sig2 := c.Classify(e2)
	if sig2 != nil {
		t.Fatalf("duplicate within window should be suppressed, got %+v", sig2)
	}
}

func TestClassify_Dedup_AfterWindowExpires(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     5,
		MaxLinesPerCycle: 10000,
	}, nopLog)
	ts := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

	e1 := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Message = "deadlock detected"
	})
	e2 := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts.Add(10 * time.Second) // beyond the 5s window
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.PID = 2000
		e.Message = "deadlock detected"
	})

	sig1 := c.Classify(e1)
	if sig1 == nil {
		t.Fatal("first occurrence should produce a signal")
	}
	sig2 := c.Classify(e2)
	if sig2 == nil {
		t.Fatal("after window expires, should produce a new signal")
	}
}

func TestClassify_Dedup_DifferentDatabase(t *testing.T) {
	c := defaultClassifier()
	ts := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

	e1 := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Database = "db1"
		e.Message = "deadlock detected"
	})
	e2 := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts.Add(1 * time.Second)
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Database = "db2"
		e.Message = "deadlock detected"
	})

	sig1 := c.Classify(e1)
	if sig1 == nil {
		t.Fatal("first db should produce signal")
	}
	sig2 := c.Classify(e2)
	if sig2 == nil {
		t.Fatal("different database should produce separate signal")
	}
}

// ---------------------------------------------------------------------------
// MaxLinesPerCycle
// ---------------------------------------------------------------------------

func TestClassify_MaxLinesPerCycle(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     1,
		MaxLinesPerCycle: 3,
	}, nopLog)

	var produced int
	for i := 0; i < 10; i++ {
		e := baseEntry(func(e *LogEntry) {
			e.Timestamp = time.Date(2024, 3, 10, 12, 0, i, 0, time.UTC)
			e.SQLState = "40P01"
			e.ErrorLevel = "ERROR"
			e.PID = 1000 + i
			e.Database = "unique_db_" + string(rune('a'+i))
			e.Message = "deadlock detected"
		})
		if c.Classify(e) != nil {
			produced++
		}
	}
	// After 3 lines processed, further entries are dropped.
	// The first produces a signal, 2nd and 3rd are deduplicated only if
	// same db. With unique dbs but only 3 lines counted, we get at most 3.
	if produced > 3 {
		t.Fatalf("expected at most 3 signals (MaxLinesPerCycle=3), got %d", produced)
	}
}

func TestClassify_MaxLinesPerCycle_ResetCycle(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     1,
		MaxLinesPerCycle: 2,
	}, nopLog)
	ts := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

	// Exhaust the cycle.
	for i := 0; i < 5; i++ {
		c.Classify(baseEntry(func(e *LogEntry) {
			e.Timestamp = ts.Add(time.Duration(i) * time.Second)
			e.SQLState = "53200"
			e.ErrorLevel = "ERROR"
			e.Database = "dbX"
			e.PID = 3000 + i
			e.Message = "out of memory"
		}))
	}

	// Reset should allow new lines.
	c.ResetCycle()
	e := baseEntry(func(e *LogEntry) {
		e.Timestamp = ts.Add(90 * time.Second) // beyond dedup window
		e.SQLState = "53200"
		e.ErrorLevel = "ERROR"
		e.Database = "dbY"
		e.PID = 9999
		e.Message = "out of memory"
	})
	sig := c.Classify(e)
	if sig == nil {
		t.Fatal("after ResetCycle, classifier should accept new lines")
	}
}

// ---------------------------------------------------------------------------
// CleanExpiredDedup
// ---------------------------------------------------------------------------

func TestCleanExpiredDedup(t *testing.T) {
	c := NewClassifier(ClassifierConfig{
		DedupWindowS:     1, // 1 second window
		MaxLinesPerCycle: 10000,
	}, nopLog)
	ts := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)

	// Insert a signal.
	c.Classify(baseEntry(func(e *LogEntry) {
		e.Timestamp = ts
		e.SQLState = "40P01"
		e.ErrorLevel = "ERROR"
		e.Message = "deadlock detected"
	}))

	// The dedup entry is keyed on timestamp inside, but CleanExpiredDedup
	// uses time.Now(). We can't control that, but we can verify the method
	// doesn't panic and that entries with old firstSeen are removed.
	// Add an entry manually with an old timestamp.
	c.mu.Lock()
	c.dedupBuf["test_old\x00testdb"] = &dedupEntry{
		firstSeen: time.Now().Add(-10 * time.Minute),
		count:     1,
		pids:      map[int]bool{1: true},
	}
	c.mu.Unlock()

	c.CleanExpiredDedup()

	c.mu.Lock()
	_, exists := c.dedupBuf["test_old\x00testdb"]
	c.mu.Unlock()
	if exists {
		t.Fatal("expired dedup entry should have been removed")
	}
}

// ---------------------------------------------------------------------------
// parseTempFileSize
// ---------------------------------------------------------------------------

func TestParseTempFileSize_Valid(t *testing.T) {
	got := parseTempFileSize("temporary file: path /tmp/pg_sort.123, size 12345")
	if got != 12345 {
		t.Fatalf("parseTempFileSize = %d, want 12345", got)
	}
}

func TestParseTempFileSize_NoMatch(t *testing.T) {
	got := parseTempFileSize("some other message")
	if got != 0 {
		t.Fatalf("parseTempFileSize = %d, want 0 for no match", got)
	}
}

func TestParseTempFileSize_EmptyMessage(t *testing.T) {
	got := parseTempFileSize("")
	if got != 0 {
		t.Fatalf("parseTempFileSize = %d, want 0 for empty", got)
	}
}

// ---------------------------------------------------------------------------
// parseSlowQueryDuration
// ---------------------------------------------------------------------------

func TestParseSlowQueryDuration_Valid(t *testing.T) {
	got := parseSlowQueryDuration("duration: 123.456 ms  statement: SELECT 1")
	if got != 123.456 {
		t.Fatalf("parseSlowQueryDuration = %f, want 123.456", got)
	}
}

func TestParseSlowQueryDuration_NoMatch(t *testing.T) {
	got := parseSlowQueryDuration("some other message")
	if got != 0 {
		t.Fatalf("parseSlowQueryDuration = %f, want 0 for no match", got)
	}
}

func TestParseSlowQueryDuration_EmptyMessage(t *testing.T) {
	got := parseSlowQueryDuration("")
	if got != 0 {
		t.Fatalf("parseSlowQueryDuration = %f, want 0 for empty", got)
	}
}

func TestParseSlowQueryDuration_WholeNumber(t *testing.T) {
	got := parseSlowQueryDuration("duration: 5000 ms")
	if got != 5000 {
		t.Fatalf("parseSlowQueryDuration = %f, want 5000", got)
	}
}

// ---------------------------------------------------------------------------
// levelRank
// ---------------------------------------------------------------------------

func TestLevelRank(t *testing.T) {
	cases := []struct {
		level string
		want  int
	}{
		{"LOG", 1},
		{"WARNING", 2},
		{"ERROR", 3},
		{"FATAL", 4},
		{"PANIC", 5},
		{"log", 1},   // case insensitive
		{"DEBUG", 0},  // unknown
		{"NOTICE", 0}, // unknown
		{"", 0},
	}
	for _, tc := range cases {
		got := levelRank(tc.level)
		if got != tc.want {
			t.Errorf("levelRank(%q) = %d, want %d", tc.level, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Classify — no match
// ---------------------------------------------------------------------------

func TestClassify_NoMatch(t *testing.T) {
	c := defaultClassifier()
	e := baseEntry(func(e *LogEntry) {
		e.SQLState = ""
		e.ErrorLevel = "ERROR"
		e.Message = "relation does not exist"
	})
	sig := c.Classify(e)
	if sig != nil {
		t.Fatalf("expected nil for unrecognized error, got %+v", sig)
	}
}

func TestClassify_EmptyEntry(t *testing.T) {
	c := defaultClassifier()
	sig := c.Classify(LogEntry{})
	if sig != nil {
		t.Fatalf("expected nil for empty entry, got %+v", sig)
	}
}
