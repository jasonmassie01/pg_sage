package logwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// nopTailerLog is a no-op logger for tailer tests.
func nopTailerLog(string, string, ...any) {}

// ---------------------------------------------------------------------------
// extensionForFormat
// ---------------------------------------------------------------------------

func TestExtensionForFormat_Jsonlog(t *testing.T) {
	if got := extensionForFormat("jsonlog"); got != ".json" {
		t.Fatalf("extensionForFormat(jsonlog) = %q, want %q", got, ".json")
	}
}

func TestExtensionForFormat_Csvlog(t *testing.T) {
	if got := extensionForFormat("csvlog"); got != ".csv" {
		t.Fatalf("extensionForFormat(csvlog) = %q, want %q", got, ".csv")
	}
}

func TestExtensionForFormat_Unknown(t *testing.T) {
	if got := extensionForFormat("syslog"); got != ".log" {
		t.Fatalf("extensionForFormat(syslog) = %q, want %q", got, ".log")
	}
}

func TestExtensionForFormat_Empty(t *testing.T) {
	if got := extensionForFormat(""); got != ".log" {
		t.Fatalf("extensionForFormat(\"\") = %q, want %q", got, ".log")
	}
}

// ---------------------------------------------------------------------------
// findLatestFile
// ---------------------------------------------------------------------------

func TestFindLatestFile_SingleFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "postgresql.json")
	if err := os.WriteFile(f, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := findLatestFile(dir, "jsonlog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != f {
		t.Fatalf("findLatestFile = %q, want %q", got, f)
	}
}

func TestFindLatestFile_MultipleFiles_ReturnsNewest(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "pg-old.json")
	if err := os.WriteFile(old, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Ensure the second file has a later mtime.
	// Use Chtimes to guarantee different timestamps.
	oldTime := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	newf := filepath.Join(dir, "pg-new.json")
	if err := os.WriteFile(newf, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := findLatestFile(dir, "jsonlog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != newf {
		t.Fatalf("findLatestFile = %q, want newest %q", got, newf)
	}
}

func TestFindLatestFile_NoFiles(t *testing.T) {
	dir := t.TempDir()
	_, err := findLatestFile(dir, "jsonlog")
	if err == nil {
		t.Fatal("expected error for empty directory, got nil")
	}
	if err != os.ErrNotExist {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}

func TestFindLatestFile_WrongExtensionIgnored(t *testing.T) {
	dir := t.TempDir()
	// Create a .csv file, but search for jsonlog (.json).
	f := filepath.Join(dir, "postgresql.csv")
	if err := os.WriteFile(f, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := findLatestFile(dir, "jsonlog")
	if err == nil {
		t.Fatal("expected error when no matching extension found")
	}
}

func TestFindLatestFile_InvalidDir(t *testing.T) {
	_, err := findLatestFile("/nonexistent/path/12345", "jsonlog")
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

// ---------------------------------------------------------------------------
// splitLines
// ---------------------------------------------------------------------------

func TestSplitLines_CompleteLines(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	raw := []byte("line1\nline2\nline3\n")
	lines := tailer.splitLines(raw)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	expects := []string{"line1", "line2", "line3"}
	for i, want := range expects {
		if string(lines[i]) != want {
			t.Errorf("line[%d] = %q, want %q", i, string(lines[i]), want)
		}
	}
}

func TestSplitLines_PartialTrailingLine(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	raw := []byte("line1\npartial")
	lines := tailer.splitLines(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 complete line, got %d", len(lines))
	}
	if string(lines[0]) != "line1" {
		t.Errorf("line[0] = %q, want %q", string(lines[0]), "line1")
	}
	// Partial should be saved.
	if string(tailer.partial) != "partial" {
		t.Errorf("partial = %q, want %q", string(tailer.partial), "partial")
	}
	// Second call with continuation should complete the partial line.
	lines2 := tailer.splitLines([]byte("_rest\n"))
	if len(lines2) != 1 {
		t.Fatalf("expected 1 line from partial completion, got %d", len(lines2))
	}
	if string(lines2[0]) != "partial_rest" {
		t.Errorf("completed line = %q, want %q", string(lines2[0]), "partial_rest")
	}
}

func TestSplitLines_OversizedLine(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 10, nopTailerLog)
	// Line exceeding maxLineLen=10 should be discarded.
	raw := []byte("short\nthisisaverylonglinethatexceedsthemaximum\nok\n")
	lines := tailer.splitLines(raw)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (oversized discarded), got %d", len(lines))
	}
	if string(lines[0]) != "short" {
		t.Errorf("line[0] = %q, want %q", string(lines[0]), "short")
	}
	if string(lines[1]) != "ok" {
		t.Errorf("line[1] = %q, want %q", string(lines[1]), "ok")
	}
}

func TestSplitLines_EmptyLinesSkipped(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	raw := []byte("line1\n\n\nline2\n")
	lines := tailer.splitLines(raw)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (empty skipped), got %d", len(lines))
	}
	if string(lines[0]) != "line1" {
		t.Errorf("line[0] = %q, want %q", string(lines[0]), "line1")
	}
	if string(lines[1]) != "line2" {
		t.Errorf("line[1] = %q, want %q", string(lines[1]), "line2")
	}
}

func TestSplitLines_WindowsLineEndings(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	raw := []byte("line1\r\nline2\r\n")
	lines := tailer.splitLines(raw)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	for i, line := range lines {
		s := string(line)
		if s[len(s)-1] == '\r' {
			t.Errorf("line[%d] = %q has trailing \\r", i, s)
		}
	}
	if string(lines[0]) != "line1" {
		t.Errorf("line[0] = %q, want %q", string(lines[0]), "line1")
	}
}

func TestSplitLines_EmptyInput(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	lines := tailer.splitLines(nil)
	if lines != nil {
		t.Fatalf("expected nil for empty input, got %v", lines)
	}
}

// ---------------------------------------------------------------------------
// Tailer.Start + ReadLines
// ---------------------------------------------------------------------------

func TestTailer_StartAndReadLines(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "pg.json")
	if err := os.WriteFile(logFile, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	tailer := NewTailer(dir, "jsonlog", 50*time.Millisecond, 65536, nopTailerLog)
	// openLatest manually since Start kicks off a goroutine.
	if err := tailer.openLatest(); err != nil {
		t.Fatalf("openLatest: %v", err)
	}
	defer tailer.Stop()

	// Write lines to the file after tailer is open.
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"line\":1}\n{\"line\":2}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	lines := tailer.ReadLines()
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if string(lines[0]) != "{\"line\":1}" {
		t.Errorf("line[0] = %q", string(lines[0]))
	}
	if string(lines[1]) != "{\"line\":2}" {
		t.Errorf("line[1] = %q", string(lines[1]))
	}
}

// ---------------------------------------------------------------------------
// Truncation detection
// ---------------------------------------------------------------------------

func TestTailer_TruncationDetection(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "pg.json")
	// Write initial data.
	if err := os.WriteFile(logFile, []byte("initial line1\ninitial line2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tailer := NewTailer(dir, "jsonlog", 50*time.Millisecond, 65536, nopTailerLog)
	if err := tailer.openLatest(); err != nil {
		t.Fatalf("openLatest: %v", err)
	}
	defer tailer.Stop()

	// Read the initial data.
	lines := tailer.ReadLines()
	if len(lines) != 2 {
		t.Fatalf("expected 2 initial lines, got %d", len(lines))
	}

	// Simulate copytruncate: truncate the file and write new data.
	if err := os.WriteFile(logFile, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	// Re-open the file handle because os.WriteFile creates a new file.
	// The tailer uses the old handle but detects truncation via stat.
	tailer.mu.Lock()
	tailer.file.Close()
	newF, err := os.Open(logFile)
	if err != nil {
		t.Fatal(err)
	}
	tailer.file = newF
	tailer.mu.Unlock()

	// Write new content.
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("after truncation\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	lines = tailer.ReadLines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line after truncation, got %d", len(lines))
	}
	if string(lines[0]) != "after truncation" {
		t.Errorf("line = %q, want %q", string(lines[0]), "after truncation")
	}
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

func TestTailer_Stop_NilFile(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	// file is nil — Stop should not panic.
	tailer.Stop()
}

func TestTailer_Stop_ClosesFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "pg.json")
	if err := os.WriteFile(logFile, []byte("data\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tailer := NewTailer(dir, "jsonlog", time.Second, 65536, nopTailerLog)
	if err := tailer.openLatest(); err != nil {
		t.Fatal(err)
	}
	tailer.Stop()
	// After Stop, file should be nil.
	if tailer.file != nil {
		t.Fatal("expected file to be nil after Stop")
	}
}

func TestTailer_ReadLines_NilFile(t *testing.T) {
	tailer := NewTailer(".", "jsonlog", time.Second, 1024, nopTailerLog)
	lines := tailer.ReadLines()
	if lines != nil {
		t.Fatalf("expected nil from ReadLines with nil file, got %v", lines)
	}
}

// ---------------------------------------------------------------------------
// NewTailer fields
// ---------------------------------------------------------------------------

func TestNewTailer_Fields(t *testing.T) {
	tailer := NewTailer("/var/log/pg", "csvlog", 2*time.Second, 4096, nopTailerLog)
	if tailer.dir != "/var/log/pg" {
		t.Errorf("dir = %q, want %q", tailer.dir, "/var/log/pg")
	}
	if tailer.format != "csvlog" {
		t.Errorf("format = %q, want %q", tailer.format, "csvlog")
	}
	if tailer.pollInterval != 2*time.Second {
		t.Errorf("pollInterval = %v, want 2s", tailer.pollInterval)
	}
	if tailer.maxLineLen != 4096 {
		t.Errorf("maxLineLen = %d, want 4096", tailer.maxLineLen)
	}
}
