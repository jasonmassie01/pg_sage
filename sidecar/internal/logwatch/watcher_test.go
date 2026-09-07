package logwatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// NewFileWatcher
// ---------------------------------------------------------------------------

func TestNewFileWatcher_CreatesWithoutError(t *testing.T) {
	cfg := config.LogWatchConfig{
		LogDirectory: "/tmp",
		Format:       "jsonlog",
	}
	fw := NewFileWatcher(cfg, nopLog)
	if fw == nil {
		t.Fatal("NewFileWatcher returned nil")
	}
	if fw.cfg.LogDirectory != "/tmp" {
		t.Errorf("cfg.LogDirectory = %q, want %q", fw.cfg.LogDirectory, "/tmp")
	}
	if fw.cfg.Format != "jsonlog" {
		t.Errorf("cfg.Format = %q, want %q", fw.cfg.Format, "jsonlog")
	}
}

// ---------------------------------------------------------------------------
// Drain on nil tailer
// ---------------------------------------------------------------------------

func TestFileWatcher_Drain_NilTailer(t *testing.T) {
	cfg := config.LogWatchConfig{}
	fw := NewFileWatcher(cfg, nopLog)
	// Not started, tailer is nil.
	signals := fw.Drain()
	if signals != nil {
		t.Fatalf("expected nil from Drain before Start, got %v", signals)
	}
}

// ---------------------------------------------------------------------------
// Stop on nil tailer
// ---------------------------------------------------------------------------

func TestFileWatcher_Stop_NilTailer(t *testing.T) {
	cfg := config.LogWatchConfig{}
	fw := NewFileWatcher(cfg, nopLog)
	// Should not panic.
	fw.Stop()
}

// ---------------------------------------------------------------------------
// Full integration: jsonlog
// ---------------------------------------------------------------------------

func TestFileWatcher_Integration_Jsonlog(t *testing.T) {
	dir := t.TempDir()

	// Build a FATAL line with SQLState 53300 (connection refused).
	logLine := map[string]any{
		"timestamp":        "2024-03-10T14:30:00.000+00:00",
		"pid":              1234,
		"session_id":       "sess1",
		"database_name":    "prod",
		"user_name":        "app",
		"error_severity":   "FATAL",
		"state_code":       "53300",
		"message":          "sorry, too many clients already",
		"detail":           "",
		"hint":             "",
		"query":            "",
		"application_name": "myapp",
	}
	data, err := json.Marshal(logLine)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')

	logFile := filepath.Join(dir, "postgresql.json")
	if err := os.WriteFile(logFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := config.LogWatchConfig{
		LogDirectory:    dir,
		Format:          "jsonlog",
		PollIntervalMs:  100,
		DedupWindowS:    60,
		MaxLineLenBytes: 65536,
		MaxLinesPerCycle: 10000,
	}
	fw := NewFileWatcher(cfg, nopLog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fw.Stop()

	// Give the tailer a moment to seek into the file, then drain.
	time.Sleep(50 * time.Millisecond)
	signals := fw.Drain()

	if len(signals) == 0 {
		t.Fatal("expected at least one signal, got none")
	}

	found := false
	for _, sig := range signals {
		if sig.ID == "log_connection_refused" {
			found = true
			if sig.Severity != "critical" {
				t.Errorf("severity = %q, want %q", sig.Severity, "critical")
			}
			db, _ := sig.Metrics["database"].(string)
			if db != "prod" {
				t.Errorf("database = %q, want %q", db, "prod")
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected log_connection_refused signal, got %v", signals)
	}
}

// ---------------------------------------------------------------------------
// Full integration: csvlog
// ---------------------------------------------------------------------------

func TestFileWatcher_Integration_Csvlog(t *testing.T) {
	dir := t.TempDir()

	// Build a CSV line: 23 columns, FATAL, SQLState 53300.
	// Use the Go csv writer for correctness.
	rec := make([]string, 23)
	rec[csvColTime] = "2024-03-10 14:30:00.123 UTC"
	rec[csvColUser] = "app"
	rec[csvColDatabase] = "prod"
	rec[csvColPID] = "5678"
	rec[csvColSessionID] = "sess2"
	rec[csvColSeverity] = "FATAL"
	rec[csvColSQLState] = "53300"
	rec[csvColMessage] = "sorry, too many clients already"
	rec[csvColDetail] = ""
	rec[csvColHint] = ""
	rec[csvColQuery] = ""
	rec[csvColApplication] = "myapp"

	// Build a CSV string manually — encoding/csv adds \n already.
	var csvBuf []byte
	for i, field := range rec {
		if i > 0 {
			csvBuf = append(csvBuf, ',')
		}
		// Quote fields containing commas or quotes.
		csvBuf = append(csvBuf, '"')
		csvBuf = append(csvBuf, []byte(field)...)
		csvBuf = append(csvBuf, '"')
	}
	csvBuf = append(csvBuf, '\n')

	logFile := filepath.Join(dir, "postgresql.csv")
	if err := os.WriteFile(logFile, csvBuf, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := config.LogWatchConfig{
		LogDirectory:    dir,
		Format:          "csvlog",
		PollIntervalMs:  100,
		DedupWindowS:    60,
		MaxLineLenBytes: 65536,
		MaxLinesPerCycle: 10000,
	}
	fw := NewFileWatcher(cfg, nopLog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fw.Stop()

	time.Sleep(50 * time.Millisecond)
	signals := fw.Drain()

	if len(signals) == 0 {
		t.Fatal("expected at least one signal from csvlog, got none")
	}

	found := false
	for _, sig := range signals {
		if sig.ID == "log_connection_refused" {
			found = true
			if sig.Severity != "critical" {
				t.Errorf("severity = %q, want %q", sig.Severity, "critical")
			}
			db, _ := sig.Metrics["database"].(string)
			if db != "prod" {
				t.Errorf("database = %q, want %q", db, "prod")
			}
			break
		}
	}
	if !found {
		ids := make([]string, len(signals))
		for i, s := range signals {
			ids[i] = s.ID
		}
		t.Fatalf("expected log_connection_refused signal, got IDs: %v", ids)
	}
}

// ---------------------------------------------------------------------------
// Start with invalid directory
// ---------------------------------------------------------------------------

func TestFileWatcher_Start_InvalidDir(t *testing.T) {
	cfg := config.LogWatchConfig{
		LogDirectory: "/nonexistent/dir/12345",
		Format:       "jsonlog",
	}
	fw := NewFileWatcher(cfg, nopLog)
	ctx := context.Background()
	err := fw.Start(ctx)
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got nil")
	}
}

// ---------------------------------------------------------------------------
// Default format
// ---------------------------------------------------------------------------

func TestFileWatcher_DefaultFormat(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "postgresql.json")
	line := `{"timestamp":"2024-03-10T14:30:00.000+00:00","pid":1,"error_severity":"ERROR","state_code":"53200","message":"out of memory"}`
	if err := os.WriteFile(logFile, []byte(line+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Format is empty — should default to jsonlog.
	cfg := config.LogWatchConfig{
		LogDirectory:    dir,
		Format:          "",
		PollIntervalMs:  100,
		MaxLinesPerCycle: 10000,
	}
	fw := NewFileWatcher(cfg, nopLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fw.Stop()

	time.Sleep(50 * time.Millisecond)
	signals := fw.Drain()
	if len(signals) == 0 {
		t.Fatal("expected signal with default format, got none")
	}
	if signals[0].ID != "log_out_of_memory" {
		t.Errorf("signal ID = %q, want %q", signals[0].ID, "log_out_of_memory")
	}
}
