package logwatch

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// deadlockCSVRecord is a PG14+ csvlog record exactly as PostgreSQL writes
// it: text fields quoted, numeric/timestamp fields bare, and a DETAIL that
// spans several physical lines inside one quoted field.
const deadlockCSVRecord = `2024-03-10 14:30:00.123 UTC,"app","prod",5678,` +
	`"10.0.0.1:51234",65edc3a8.162e,3,"UPDATE",2024-03-10 14:29:59 UTC,3/7,742,` +
	`ERROR,40P01,"deadlock detected","Process 5678 waits for ShareLock on ` +
	`transaction 743; blocked by process 5679.
Process 5679 waits for ShareLock on transaction 742; blocked by process 5678.
Process 5678: UPDATE t SET v = 1 WHERE id = 1;
Process 5679: UPDATE t SET v = 2 WHERE id = 2;","See server log for query details.",,,` +
	`"while updating tuple (0,1) in relation ""t""","UPDATE t
   SET v = 1 WHERE id = 1;",,,"myapp","client backend",,0
`

const followingCSVRecord = `2024-03-10 14:31:00.000 UTC,"app","prod",5680,` +
	`"10.0.0.1:51235",65edc3a8.1630,1,"SELECT",2024-03-10 14:30:59 UTC,4/2,0,` +
	`FATAL,53300,"sorry, too many clients already",,,,,,,,,"myapp","client backend",,0
`

func parseCSVRecordBytes(t *testing.T, rec []byte) []string {
	t.Helper()
	r := csv.NewReader(bytes.NewReader(rec))
	r.FieldsPerRecord = -1
	fields, err := r.Read()
	if err != nil {
		t.Fatalf("record %q is not valid CSV: %v", rec, err)
	}
	return fields
}

// G1-B03: a quoted newline must not terminate a csvlog record, even when
// the record is split across two reads.
func TestSplitLines_CSVQuotedNewlinesAcrossReads(t *testing.T) {
	tl := NewTailer("", "csvlog", time.Second, 65536, nil)
	all := []byte(deadlockCSVRecord + followingCSVRecord)
	cut := strings.Index(deadlockCSVRecord, "Process 5678: UPDATE")
	first := tl.splitLines(all[:cut])
	if len(first) != 0 {
		t.Fatalf("partial quoted record emitted early: %q", first)
	}
	records := tl.splitLines(all[cut:])
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2: %q", len(records), records)
	}
	fields := parseCSVRecordBytes(t, records[0])
	if len(fields) != csvExtColumns {
		t.Fatalf("deadlock record has %d fields, want %d", len(fields), csvExtColumns)
	}
	if !strings.Contains(fields[csvColDetail], "Process 5679 waits for ShareLock") {
		t.Errorf("detail lost its continuation lines: %q", fields[csvColDetail])
	}
	if !strings.Contains(fields[csvColQuery], "SET v = 1") {
		t.Errorf("multi-line statement truncated: %q", fields[csvColQuery])
	}
	second := parseCSVRecordBytes(t, records[1])
	if second[csvColSQLState] != "53300" {
		t.Errorf("second record SQLSTATE = %q, want 53300", second[csvColSQLState])
	}
}

// G1-B03: starting mid-record (startup lookback lands inside a quoted
// field) must resynchronise at the next record that starts with a
// timestamp instead of inverting quote parity for the rest of the file.
func TestSplitLines_CSVResyncAfterMidRecordStart(t *testing.T) {
	tl := NewTailer("", "csvlog", time.Second, 65536, nil)
	cut := strings.Index(deadlockCSVRecord, "Process 5679: UPDATE")
	tail := deadlockCSVRecord[cut:]
	records := tl.splitLines([]byte(tail + followingCSVRecord + deadlockCSVRecord))
	var parsed []LogEntry
	for _, rec := range records {
		r := csv.NewReader(bytes.NewReader(rec))
		r.FieldsPerRecord = -1
		fields, err := r.Read()
		if err != nil {
			continue
		}
		if entry, perr := ParseCSVLogLine(fields); perr == nil {
			parsed = append(parsed, entry)
		}
	}
	if len(parsed) != 2 {
		t.Fatalf("parsed %d complete records after resync, want 2", len(parsed))
	}
	if parsed[0].SQLState != "53300" || parsed[1].SQLState != "40P01" {
		t.Errorf("resync records = %q/%q, want 53300/40P01",
			parsed[0].SQLState, parsed[1].SQLState)
	}
}

// G1-B03 end to end: a deadlock written by PostgreSQL's csvlog must reach
// the classifier as a log_deadlock_detected signal with its full detail.
func TestFileWatcher_CsvlogMultiLineDeadlock(t *testing.T) {
	dir := t.TempDir()
	data := []byte(deadlockCSVRecord + followingCSVRecord)
	if err := os.WriteFile(filepath.Join(dir, "postgresql.csv"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{
		LogDirectory: dir, Format: "csvlog", PollIntervalMs: 100,
		DedupWindowS: 60, MaxLineLenBytes: 65536, MaxLinesPerCycle: 10000,
	}, nopLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fw.Stop()
	signals := fw.Drain()
	ids := map[string]string{}
	for _, sig := range signals {
		detail, _ := sig.Metrics["detail"].(string)
		ids[sig.ID] = detail
	}
	detail, ok := ids["log_deadlock_detected"]
	if !ok {
		t.Fatalf("no deadlock signal from multi-line csvlog record; got %v", ids)
	}
	if !strings.Contains(detail, "Process 5679 waits") {
		t.Errorf("deadlock detail truncated: %q", detail)
	}
	if _, ok := ids["log_connection_refused"]; !ok {
		t.Errorf("record after the multi-line record was lost; got %v", ids)
	}
}

// G1-B24/B02: lines that fail to parse must be reported, not silently
// dropped at debug level.
func TestFileWatcher_ReportsUnparseableLines(t *testing.T) {
	dir := t.TempDir()
	bad := `{"timestamp":"2024-03-10 14:30:00.123 XYZT","pid":1,` +
		`"error_severity":"FATAL","state_code":"53300","message":"x"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "postgresql.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	logFn := func(level, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if strings.EqualFold(level, "warn") {
			warnings = append(warnings, fmt.Sprintf(msg, args...))
		}
	}
	fw := NewFileWatcher(config.LogWatchConfig{
		LogDirectory: dir, Format: "jsonlog", PollIntervalMs: 100,
		MaxLineLenBytes: 65536, MaxLinesPerCycle: 10000,
	}, logFn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fw.Stop()
	if sigs := fw.Drain(); len(sigs) != 0 {
		t.Fatalf("unparseable line produced signals: %v", sigs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "|"), "XYZT") {
		t.Fatalf("parse failure not reported at WARN; warnings = %q", warnings)
	}
}
