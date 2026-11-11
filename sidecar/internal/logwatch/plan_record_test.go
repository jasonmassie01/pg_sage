package logwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// rca-04: an auto_explain plan record (its message starts with the
// "duration: ... plan:" header) may be up to 1 MiB, the size the plan
// parser accepts; every other record keeps logwatch.max_line_len_bytes.
// A record dropped for its size is counted, never silently lost. No
// concurrent-access test beyond the watcher's: the tailer's counters are
// read and reset under its own mutex like TakeDropped.

const testMaxLine = 64 << 10

// csvPlanRecord is a csvlog auto_explain record of exactly size bytes
// (without its newline): the JSON plan is padded inside a quoted field.
func csvPlanRecord(t *testing.T, size int) string {
	t.Helper()
	head := `2026-11-11 10:00:00.000 UTC,"app","appdb",42,"10.0.0.1:5000",` +
		`6551a000.2a,1,"SELECT",2026-11-11 09:59:00 UTC,3/7,0,LOG,00000,` +
		`"duration: 1.500 ms  plan:` + "\n" + `{""Query Text"": ""SELECT 1"", ""Pad"": ""`
	tail := `"", ""Plan"": {""Total Cost"": 1}}",,,,,,,,,"psql","client backend",,0`
	pad := size - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("record size %d is below the fixed %d bytes", size, len(head)+len(tail))
	}
	return head + strings.Repeat("x", pad) + tail
}

// csvPlainRecord is a csvlog record of exactly size bytes that is not a plan.
func csvPlainRecord(size int) string {
	head := `2026-11-11 10:00:00.000 UTC,"app","appdb",42,"10.0.0.1:5000",` +
		`6551a000.2a,1,"SELECT",2026-11-11 09:59:00 UTC,3/7,0,LOG,00000,"duration: 1 ms  ` +
		`statement: SELECT '`
	tail := `'",,,,,,,,,"psql","client backend",,0`
	return head + strings.Repeat("y", size-len(head)-len(tail)) + tail
}

// splitInChunks feeds raw to splitLines in chunk-sized reads, as the pump does.
func splitInChunks(tl *Tailer, raw string, chunk int) [][]byte {
	var out [][]byte
	for start := 0; start < len(raw); start += chunk {
		end := min(start+chunk, len(raw))
		out = append(out, tl.splitLines([]byte(raw[start:end]))...)
	}
	return out
}

func TestSplitLines_PlanRecordSizeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		kept      bool
		plainKept bool
	}{
		{"64 KiB", testMaxLine, true, true},
		{"64 KiB + 1", testMaxLine + 1, true, false},
		{"exactly 1 MiB", PlanRecordMaxBytes, true, false},
		{"1 MiB + 1", PlanRecordMaxBytes + 1, false, false},
	} {
		tl := NewTailer(".", "csvlog", time.Second, testMaxLine, nopTailerLog)
		follow := csvPlainRecord(200)
		lines := splitInChunks(tl, csvPlanRecord(t, tc.size)+"\n"+follow+"\n", 256<<10)
		wantLines, wantDropped := 1, 1
		if tc.kept {
			wantLines, wantDropped = 2, 0
		}
		if len(lines) != wantLines || tl.TakeOversized() != wantDropped {
			t.Fatalf("%s plan record: %d lines, want %d (dropped counted %d)", tc.name,
				len(lines), wantLines, wantDropped)
		}
		if tc.kept && len(lines[0]) != tc.size {
			t.Fatalf("%s plan record kept with %d bytes", tc.name, len(lines[0]))
		}
		if string(lines[len(lines)-1]) != follow {
			t.Fatalf("%s: the record after the plan was lost", tc.name)
		}
		if tl.TakeOversized() != 0 {
			t.Fatalf("%s: TakeOversized did not reset", tc.name)
		}
		plain := NewTailer(".", "csvlog", time.Second, testMaxLine, nopTailerLog)
		got := splitInChunks(plain, csvPlainRecord(tc.size)+"\n", 256<<10)
		if (len(got) == 1) != tc.plainKept {
			t.Fatalf("%s plain record: kept = %v, want %v", tc.name, len(got) == 1, tc.plainKept)
		}
	}
}

// jsonlog keeps the same allowance: the header follows "message":".
func TestSplitLines_JSONLogPlanRecordOver64KiB(t *testing.T) {
	msg := `duration: 2.000 ms  plan:\n{\"Query Text\": \"SELECT 1\", \"Pad\": \"` +
		strings.Repeat("z", 200<<10) + `\", \"Plan\": {\"Total Cost\": 1}}`
	line := `{"timestamp":"2026-11-11 10:00:00.000 UTC","dbname":"appdb",` +
		`"error_severity":"LOG","message":"` + msg + `"}`
	tl := NewTailer(".", "jsonlog", time.Second, testMaxLine, nopTailerLog)
	lines := splitInChunks(tl, line+"\n", 64<<10)
	if len(lines) != 1 || len(lines[0]) != len(line) || tl.TakeOversized() != 0 {
		t.Fatalf("jsonlog plan record over 64 KiB: %d lines", len(lines))
	}
	plain := `{"timestamp":"2026-11-11 10:00:00.000 UTC","message":"` +
		strings.Repeat("q", testMaxLine) + `"}`
	if got := splitInChunks(tl, plain+"\n", 64<<10); len(got) != 0 || tl.TakeOversized() != 1 {
		t.Fatalf("plain jsonlog line over 64 KiB: %d lines kept", len(got))
	}
}

// Integration through the watcher: the 1 MiB plan reaches entry
// subscribers whole, and the oversized drop shows in the source status.
func TestFileWatcher_PlanRecordsReachSubscribersAndDropsAreCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "postgresql.csv")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 3600000, MaxLineLenBytes: testMaxLine}, nil)
	if err := fw.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(fw.Stop)
	sub := fw.SubscribeEntries("plans:test", "appdb")
	body := csvPlanRecord(t, PlanRecordMaxBytes) + "\n" +
		csvPlanRecord(t, PlanRecordMaxBytes+1) + "\n" + csvPlainRecord(300) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fw.Drain()
	entries := sub.Drain()
	if len(entries) != 2 || !strings.HasPrefix(entries[0].Message, "duration: 1.500 ms  plan:") ||
		len(entries[0].Message) < PlanRecordMaxBytes-400 {
		t.Fatalf("subscriber got %d entries; want the 1 MiB plan and the plain record",
			len(entries))
	}
	if got := fw.DroppedLines().Oversized; got != 1 {
		t.Fatalf("DroppedLines().Oversized = %d, want 1", got)
	}
	if st := fw.Status(); st.DroppedLines != 1 {
		t.Fatalf("status DroppedLines = %d, want 1 (the 1 MiB + 1 plan)", st.DroppedLines)
	}
}
