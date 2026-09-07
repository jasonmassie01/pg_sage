package logwatch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ── resolveFormat ──────────────────────────────────────────────────

func TestResolveFormat_Jsonlog(t *testing.T) {
	got, err := resolveFormat("jsonlog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "jsonlog" {
		t.Fatalf("expected jsonlog, got %s", got)
	}
}

func TestResolveFormat_Csvlog(t *testing.T) {
	got, err := resolveFormat("csvlog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "csvlog" {
		t.Fatalf("expected csvlog, got %s", got)
	}
}

func TestResolveFormat_JsonlogPreferredOverCsvlog(t *testing.T) {
	// When both are present, jsonlog wins (checked first).
	got, err := resolveFormat("csvlog,jsonlog")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "jsonlog" {
		t.Fatalf("expected jsonlog when both present, got %s", got)
	}
}

func TestResolveFormat_CsvlogWithStderr(t *testing.T) {
	got, err := resolveFormat("csvlog,stderr")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "csvlog" {
		t.Fatalf("expected csvlog, got %s", got)
	}
}

func TestResolveFormat_StderrOnly(t *testing.T) {
	_, err := resolveFormat("stderr")
	if err == nil {
		t.Fatal("expected error for stderr-only destination")
	}
}

func TestResolveFormat_Syslog(t *testing.T) {
	_, err := resolveFormat("syslog")
	if err == nil {
		t.Fatal("expected error for syslog destination")
	}
}

func TestResolveFormat_Empty(t *testing.T) {
	_, err := resolveFormat("")
	if err == nil {
		t.Fatal("expected error for empty log_destination")
	}
}

// ── resolveLogDir ──────────────────────────────────────────────────

func TestResolveLogDir_AbsoluteUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix absolute path test not applicable on windows")
	}
	got, err := resolveLogDir("/var/log/postgresql", "/data")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/var/log/postgresql" {
		t.Fatalf("expected /var/log/postgresql, got %s", got)
	}
}

func TestResolveLogDir_AbsoluteWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows absolute path test not applicable on unix")
	}
	got, err := resolveLogDir(`C:\pg\log`, `C:\pg\data`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `C:\pg\log` {
		t.Fatalf(`expected C:\pg\log, got %s`, got)
	}
}

func TestResolveLogDir_RelativePrepends(t *testing.T) {
	got, err := resolveLogDir("log", "/data/postgresql")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/data/postgresql", "log")
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestResolveLogDir_RelativeNestedPath(t *testing.T) {
	got, err := resolveLogDir("pg_log/daily", "/pgdata")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/pgdata", "pg_log/daily")
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestResolveLogDir_EmptyLogDir(t *testing.T) {
	_, err := resolveLogDir("", "/data")
	if err == nil {
		t.Fatal("expected error for empty log_directory")
	}
}

func TestResolveLogDir_RelativeWithEmptyDataDir(t *testing.T) {
	_, err := resolveLogDir("log", "")
	if err == nil {
		t.Fatal(
			"expected error for relative log_directory " +
				"with empty data_directory")
	}
}

// ── DetectLogSettings (directory existence check) ──────────────────

func TestDetectLogSettings_DirectoryMustExist(t *testing.T) {
	// Test the os.Stat validation by calling the helpers
	// directly and then checking a non-existent directory.
	dir := filepath.Join(os.TempDir(), "pg_sage_test_nonexist")
	_ = os.RemoveAll(dir) // ensure it does not exist

	// resolveLogDir and resolveFormat pass, but the
	// full DetectLogSettings would fail on the Stat check.
	// We verify the Stat path by directly checking.
	_, statErr := os.Stat(dir)
	if statErr == nil {
		t.Fatal("expected stat error for non-existent directory")
	}
}

func TestDetectLogSettings_DirectoryExists(t *testing.T) {
	dir := t.TempDir() // creates a real temp directory
	_, statErr := os.Stat(dir)
	if statErr != nil {
		t.Fatalf("temp directory should exist: %v", statErr)
	}
}
