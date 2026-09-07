package logwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

func TestR14StartedTailerDoesNotDiscardAppendedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.json")
	writeLogFile(t, path, "")

	tailer := NewTailer(dir, "jsonlog", 5*time.Millisecond, 1024, wave4NopLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tailer.Start(ctx); err != nil {
		t.Fatalf("start tailer: %v", err)
	}
	defer tailer.Stop()

	appendLogText(t, path, "event-one\nevent-two\n")
	time.Sleep(50 * time.Millisecond)
	requireLogLines(t, tailer.ReadLines(), "event-one", "event-two")
	requireLogLines(t, tailer.ReadLines())
}

func TestR14AppendIsDeliveredExactlyOnce(t *testing.T) {
	tailer, path := openTestTailer(t)
	appendLogText(t, path, "first\nsecond\n")

	requireLogLines(t, tailer.ReadLines(), "first", "second")
	requireLogLines(t, tailer.ReadLines())
	appendLogText(t, path, "third\n")
	requireLogLines(t, tailer.ReadLines(), "third")
	requireLogLines(t, tailer.ReadLines())
}

func TestR14RotationDrainsOldFileBeforeNewFileExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "postgresql-old.json")
	writeLogFile(t, oldPath, "")
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("set old log mtime: %v", err)
	}
	tailer := NewTailer(dir, "jsonlog", time.Hour, 1024, wave4NopLog)
	if err := tailer.openLatest(); err != nil {
		t.Fatalf("open old log: %v", err)
	}
	defer tailer.Stop()

	appendLogText(t, oldPath, "old-final\n")
	newPath := filepath.Join(dir, "postgresql-new.json")
	writeLogFile(t, newPath, "new-first\n")
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatalf("set new log mtime: %v", err)
	}

	requireLogLines(t, tailer.ReadLines(), "old-final")
	appendLogText(t, newPath, "new-second\n")
	requireLogLines(t, tailer.ReadLines(), "new-first", "new-second")
	requireLogLines(t, tailer.ReadLines())
}

func TestR14FastCopytruncateLargerRewriteIsNotMissed(t *testing.T) {
	tailer, path := openTestTailer(t)
	appendLogText(t, path, "old-a\nold-b\n")
	requireLogLines(t, tailer.ReadLines(), "old-a", "old-b")

	writeLogFile(t, path, "new-a\nnew-b\nnew-c\n")
	requireLogLines(t, tailer.ReadLines(), "new-a", "new-b", "new-c")
	requireLogLines(t, tailer.ReadLines())
}

func TestR14PartialLineWaitsForTerminatorAndIsDeliveredOnce(t *testing.T) {
	tailer, path := openTestTailer(t)
	appendLogText(t, path, "partial")
	requireLogLines(t, tailer.ReadLines())

	appendLogText(t, path, "-complete\nnext\n")
	requireLogLines(t, tailer.ReadLines(), "partial-complete", "next")
	requireLogLines(t, tailer.ReadLines())
}

func TestR14TailerRawQueueOverflowRetainsNewestLines(t *testing.T) {
	tailer, path := openTestTailer(t)
	var data bytes.Buffer
	for i := 0; i < maxTailerQueueSize+5; i++ {
		fmt.Fprintf(&data, "line-%d\n", i)
	}
	appendLogBytes(t, path, data.Bytes())

	got := tailer.ReadLines()
	if len(got) != maxTailerQueueSize {
		t.Fatalf("raw queue = %d, want %d", len(got), maxTailerQueueSize)
	}
	if string(got[0]) != "line-5" || string(got[len(got)-1]) != "line-10004" {
		t.Fatalf("bounded raw range = %q..%q", got[0], got[len(got)-1])
	}
}

func TestR14OneSourceDeliversEachSignalOnceToEverySubscriber(t *testing.T) {
	fanout, path := startTestFanout(t, maxBufferSize)
	first := fanout.Subscribe("first")
	second := fanout.Subscribe("second")
	appendOOMBatch(t, path, 0, 1)

	fanout.DrainSource()
	requireSignalDatabases(t, first.Drain(), "db-0")
	requireSignalDatabases(t, second.Drain(), "db-0")
	requireSignalDatabases(t, first.Drain())
	requireSignalDatabases(t, second.Drain())
	fanout.DrainSource()
	requireSignalDatabases(t, first.Drain())
	requireSignalDatabases(t, second.Drain())
}

func TestR14ParsedEntrySubscriberSharesOffsetAndFiltersDatabase(t *testing.T) {
	fanout, path := startTestFanout(t, maxBufferSize)
	entries := fanout.SubscribeEntries("migration", "db-1")
	appendOOMBatch(t, path, 0, 3)

	fanout.DrainSource()
	got := entries.Drain()
	if len(got) != 1 || got[0].Database != "db-1" {
		t.Fatalf("filtered entries = %+v, want only db-1", got)
	}
	if second := entries.Drain(); len(second) != 0 {
		t.Fatalf("entry delivered more than once: %+v", second)
	}
}

func TestR14ParsedEntrySubscriberBacklogIsBounded(t *testing.T) {
	fw := NewFileWatcher(config.LogWatchConfig{}, wave4NopLog)
	entries := fw.SubscribeEntries("slow", "")
	for i := 0; i < maxBufferSize+5; i++ {
		fw.publishEntry(LogEntry{Database: fmt.Sprintf("db-%d", i)})
	}

	got := entries.Drain()
	if len(got) != maxBufferSize {
		t.Fatalf("entry backlog = %d, want %d", len(got), maxBufferSize)
	}
	if got[0].Database != "db-5" || got[len(got)-1].Database != "db-10004" {
		t.Fatalf("bounded entry range = %q..%q", got[0].Database,
			got[len(got)-1].Database)
	}
}

func TestR14SlowSubscriberBacklogIsBounded(t *testing.T) {
	fanout, path := startTestFanout(t, maxBufferSize+10)
	slow := fanout.Subscribe("slow")
	fast := fanout.Subscribe("fast")
	appendOOMBatch(t, path, 0, maxBufferSize)

	fanout.DrainSource()
	firstFast := fast.Drain()
	if len(firstFast) != maxBufferSize {
		t.Fatalf("fast first batch = %d, want %d", len(firstFast), maxBufferSize)
	}
	appendOOMBatch(t, path, maxBufferSize, 5)
	fanout.DrainSource()
	requireSignalDatabases(t, fast.Drain(),
		"db-10000", "db-10001", "db-10002", "db-10003", "db-10004")

	slowBatch := slow.Drain()
	if len(slowBatch) != maxBufferSize {
		t.Fatalf("slow backlog = %d, want bounded %d", len(slowBatch), maxBufferSize)
	}
	assertSignalDatabase(t, slowBatch[0], "db-5")
	assertSignalDatabase(t, slowBatch[len(slowBatch)-1], "db-10004")
}

func TestR14CancellationStopsPumpAndStopClosesOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.json")
	writeLogFile(t, path, "")
	tailer := NewTailer(dir, "jsonlog", 5*time.Millisecond, 1024, wave4NopLog)
	ctx, cancel := context.WithCancel(context.Background())
	if err := tailer.Start(ctx); err != nil {
		t.Fatalf("start tailer: %v", err)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	appendLogText(t, path, "after-cancel\n")
	requireLogLines(t, tailer.ReadLines(), "after-cancel")

	tailer.Stop()
	tailer.Stop()
	appendLogText(t, path, "after-stop\n")
	requireLogLines(t, tailer.ReadLines())
}

func openTestTailer(t *testing.T) (*Tailer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.json")
	writeLogFile(t, path, "")
	tailer := NewTailer(dir, "jsonlog", time.Hour, 1024, wave4NopLog)
	if err := tailer.openLatest(); err != nil {
		t.Fatalf("open test log: %v", err)
	}
	t.Cleanup(tailer.Stop)
	return tailer, path
}

func startTestFanout(t *testing.T, maxLines int) (*LogFanout, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.json")
	writeLogFile(t, path, "")
	fw := NewFileWatcher(config.LogWatchConfig{
		LogDirectory: dir, Format: "jsonlog", PollIntervalMs: 3600000,
		MaxLineLenBytes: 4096, MaxLinesPerCycle: maxLines,
	}, wave4NopLog)
	ctx, cancel := context.WithCancel(context.Background())
	if err := fw.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start file watcher: %v", err)
	}
	fanout := NewLogFanout(fw)
	t.Cleanup(func() { cancel(); fanout.Stop() })
	return fanout, path
}

func appendOOMBatch(t *testing.T, path string, start, count int) {
	t.Helper()
	var buf bytes.Buffer
	for i := start; i < start+count; i++ {
		line := map[string]any{
			"timestamp": "2026-07-19T12:00:00.000+00:00",
			"pid":       i + 1, "database_name": fmt.Sprintf("db-%d", i),
			"error_severity": "ERROR", "state_code": "53200",
			"message": "out of memory",
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("marshal log line %d: %v", i, err)
		}
		buf.Write(encoded)
		buf.WriteByte('\n')
	}
	appendLogBytes(t, path, buf.Bytes())
}

func requireSignalDatabases(t *testing.T, signals []*rca.Signal, wants ...string) {
	t.Helper()
	if len(signals) != len(wants) {
		t.Fatalf("signal count = %d, want %d", len(signals), len(wants))
	}
	for i, want := range wants {
		assertSignalDatabase(t, signals[i], want)
	}
}

func assertSignalDatabase(t *testing.T, signal *rca.Signal, want string) {
	t.Helper()
	got, _ := signal.Metrics["database"].(string)
	if got != want {
		t.Fatalf("signal database = %q, want %q", got, want)
	}
}

func requireLogLines(t *testing.T, lines [][]byte, wants ...string) {
	t.Helper()
	if len(lines) != len(wants) {
		t.Fatalf("line count = %d, want %d: %q", len(lines), len(wants), lines)
	}
	for i, want := range wants {
		if got := string(lines[i]); got != want {
			t.Fatalf("line %d = %q, want %q", i, got, want)
		}
	}
}

func writeLogFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write log file: %v", err)
	}
}

func appendLogText(t *testing.T, path, content string) {
	t.Helper()
	appendLogBytes(t, path, []byte(content))
}

func appendLogBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open log for append: %v", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatalf("append log: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close appended log: %v", err)
	}
}

func wave4NopLog(string, string, ...any) {}
