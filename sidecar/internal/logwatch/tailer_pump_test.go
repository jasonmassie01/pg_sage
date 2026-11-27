package logwatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// rca-06: on overflow the raw line queue evicts the oldest non-critical
// lines first, so a PANIC or disk-full line survives a log storm, and every
// eviction is counted.

func jsonRecord(t *testing.T, severity, state, msg string, pid int) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"timestamp": "2026-11-08 12:00:00.000 UTC", "pid": pid,
		"dbname": "app", "error_severity": severity, "state_code": state,
		"message": msg,
	})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return line
}

// noiseStorm writes n ERROR lines with no signal pattern and the given
// critical record at index at.
func noiseStorm(t *testing.T, n, at int, critical []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		if i == at {
			buf.Write(critical)
			buf.WriteByte('\n')
		}
		buf.Write(jsonRecord(t, "ERROR", "42P01",
			fmt.Sprintf("relation \"t%d\" does not exist", i), 100+i))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func countContaining(lines [][]byte, sub string) int {
	n := 0
	for _, l := range lines {
		if bytes.Contains(l, []byte(sub)) {
			n++
		}
	}
	return n
}

// Boundary: 20,000 noise ERROR lines and one PANIC in one poll keep the
// PANIC and count every evicted line.
func TestTailerOverflowKeepsPanicAndCountsEvictions(t *testing.T) {
	tailer, path := openTestTailer(t)
	panicLine := jsonRecord(t, "PANIC", "XX000", "could not write WAL", 1)
	appendLogBytes(t, path, noiseStorm(t, 20000, 10, panicLine))

	got := tailer.ReadLines()
	if len(got) != maxTailerQueueSize {
		t.Fatalf("queue = %d lines, want the cap %d", len(got), maxTailerQueueSize)
	}
	if n := countContaining(got, `"error_severity":"PANIC"`); n != 1 {
		t.Fatalf("PANIC lines kept = %d, want 1", n)
	}
	if !bytes.Equal(got[0], panicLine) {
		t.Fatalf("oldest kept line = %q, want the PANIC (order preserved)", got[0])
	}
	if !bytes.Contains(got[len(got)-1], []byte(`t19999`)) {
		t.Fatalf("newest line lost: %q", got[len(got)-1])
	}
	if dropped := tailer.TakeDropped(); dropped != 20001-maxTailerQueueSize {
		t.Fatalf("dropped = %d, want %d", dropped, 20001-maxTailerQueueSize)
	}
	if again := tailer.TakeDropped(); again != 0 {
		t.Fatalf("TakeDropped did not reset: %d", again)
	}
}

// Disk full (53100) at ERROR, crash shutdown (57P02) and a FATAL survive.
func TestTailerOverflowKeepsEveryCriticalClass(t *testing.T) {
	tailer, path := openTestTailer(t)
	var data bytes.Buffer
	criticals := [][]byte{
		jsonRecord(t, "ERROR", "53100", "could not extend file: No space left", 1),
		jsonRecord(t, "FATAL", "57P02", "terminating connection", 2),
		jsonRecord(t, "ERROR", "XX001", "invalid page in block 7", 3),
	}
	for _, c := range criticals {
		data.Write(c)
		data.WriteByte('\n')
	}
	data.Write(noiseStorm(t, maxTailerQueueSize+50, -1, nil))
	appendLogBytes(t, path, data.Bytes())

	got := tailer.ReadLines()
	for _, c := range criticals {
		if countContaining(got, string(c)) != 1 {
			t.Errorf("critical line evicted: %s", c)
		}
	}
	if dropped := tailer.TakeDropped(); dropped != 53 {
		t.Fatalf("dropped = %d, want 53", dropped)
	}
}

// When every queued line is critical the oldest critical lines go, the
// queue stays bounded, and the count still matches.
func TestTailerOverflowAllCriticalDropsOldest(t *testing.T) {
	tailer, path := openTestTailer(t)
	var data bytes.Buffer
	for i := 0; i < maxTailerQueueSize+5; i++ {
		data.Write(jsonRecord(t, "PANIC", "", fmt.Sprintf("crash-%d", i), i))
		data.WriteByte('\n')
	}
	appendLogBytes(t, path, data.Bytes())

	got := tailer.ReadLines()
	if len(got) != maxTailerQueueSize {
		t.Fatalf("queue = %d, want %d", len(got), maxTailerQueueSize)
	}
	if !bytes.Contains(got[0], []byte(`"crash-5"`)) {
		t.Fatalf("oldest kept = %q, want crash-5", got[0])
	}
	if dropped := tailer.TakeDropped(); dropped != 5 {
		t.Fatalf("dropped = %d, want 5", dropped)
	}
}

// Boundary: exactly the cap is no overflow; one more line evicts one.
func TestTailerOverflowAtCapBoundary(t *testing.T) {
	tailer, path := openTestTailer(t)
	appendLogBytes(t, path, noiseStorm(t, maxTailerQueueSize, -1, nil))
	if got := len(tailer.ReadLines()); got != maxTailerQueueSize {
		t.Fatalf("queue = %d, want %d", got, maxTailerQueueSize)
	}
	if dropped := tailer.TakeDropped(); dropped != 0 {
		t.Fatalf("dropped at the cap = %d, want 0", dropped)
	}
	appendLogBytes(t, path, noiseStorm(t, maxTailerQueueSize+1, -1, nil))
	tailer.ReadLines()
	if dropped := tailer.TakeDropped(); dropped != 1 {
		t.Fatalf("dropped one over the cap = %d, want 1", dropped)
	}
}

// Overflow across several pumps before a read keeps earlier survivors and
// counts each eviction once.
func TestTailerOverflowAcrossPumpsKeepsSurvivors(t *testing.T) {
	tailer, path := openTestTailer(t)
	panicLine := jsonRecord(t, "PANIC", "", "first crash", 1)
	appendLogBytes(t, path, noiseStorm(t, maxTailerQueueSize, 0, panicLine))
	tailer.pump()
	appendLogBytes(t, path, noiseStorm(t, 3000, -1, nil))
	tailer.pump()

	got := tailer.ReadLines()
	if countContaining(got, "first crash") != 1 {
		t.Fatal("PANIC evicted by a later pump")
	}
	if dropped := tailer.TakeDropped(); dropped != 3001 {
		t.Fatalf("dropped = %d, want 3001", dropped)
	}
}

// csvlog records are split before the sniff: a PANIC with a quoted,
// multi-line message survives.
func TestTailerOverflowCSVKeepsPanic(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/postgresql.csv"
	writeLogFile(t, path, "")
	tailer := NewTailer(dir, "csvlog", 0, 4096, wave4NopLog)
	if err := tailer.openLatest(); err != nil {
		t.Fatalf("open csv log: %v", err)
	}
	t.Cleanup(tailer.Stop)
	var data bytes.Buffer
	data.WriteString(csvRecord("PANIC", "XX000", "\"crash,\nline two\""))
	for i := 0; i < maxTailerQueueSize+10; i++ {
		data.WriteString(csvRecord("ERROR", "42P01", fmt.Sprintf("noise %d", i)))
	}
	appendLogBytes(t, path, data.Bytes())

	got := tailer.ReadLines()
	if countContaining(got, "PANIC") != 1 {
		t.Fatal("csvlog PANIC evicted")
	}
	if dropped := tailer.TakeDropped(); dropped != 11 {
		t.Fatalf("dropped = %d, want 11", dropped)
	}
}

// csvRecord builds a 26-column csvlog record; msg is inserted verbatim.
func csvRecord(severity, state, msg string) string {
	cols := make([]string, 26)
	cols[0] = "2026-11-08 12:00:00.000 UTC"
	cols[3] = "42"
	cols[csvColSeverity] = severity
	cols[csvColSQLState] = state
	cols[csvColMessage] = msg
	return strings.Join(cols, ",") + "\n"
}

func TestIsCriticalRecord(t *testing.T) {
	cases := []struct {
		name   string
		format string
		line   string
		want   bool
	}{
		{"panic", "jsonlog", `{"error_severity":"PANIC","message":"x"}`, true},
		{"fatal", "jsonlog", `{"error_severity":"FATAL","state_code":"28P01"}`, true},
		{"disk full", "jsonlog", `{"error_severity":"ERROR","state_code":"53100"}`, true},
		{"oom", "jsonlog", `{"error_severity":"ERROR","state_code":"53200"}`, true},
		{"admin shutdown", "jsonlog", `{"error_severity":"ERROR","state_code":"57P01"}`, true},
		{"system error", "jsonlog", `{"error_severity":"ERROR","state_code":"58030"}`, true},
		{"data corrupted", "jsonlog", `{"error_severity":"ERROR","state_code":"XX001"}`, true},
		{"index corrupted", "jsonlog", `{"error_severity":"ERROR","state_code":"XX002"}`, true},
		{"spaced keys", "jsonlog", `{"error_severity": "PANIC"}`, true},
		{"internal error", "jsonlog", `{"error_severity":"ERROR","state_code":"XX000"}`, false},
		{"statement timeout", "jsonlog", `{"error_severity":"ERROR","state_code":"57014"}`, false},
		{"undefined table", "jsonlog", `{"error_severity":"ERROR","state_code":"42P01"}`, false},
		{"log line", "jsonlog", `{"error_severity":"LOG","message":"checkpoint"}`, false},
		{"panic in message only", "jsonlog",
			`{"error_severity":"ERROR","message":"\"error_severity\":\"PANIC\""}`, false},
		{"malformed json", "jsonlog", `{"error_severity":`, false},
		{"empty", "jsonlog", ``, false},
		{"csv panic", "csvlog", csvRecord("PANIC", "XX000", "boom"), true},
		{"csv disk full", "csvlog", csvRecord("ERROR", "53100", "no space"), true},
		{"csv noise", "csvlog", csvRecord("ERROR", "42P01", "nope"), false},
		{"csv too short", "csvlog", "2026-11-08 12:00:00 UTC,PANIC\n", false},
		{"csv broken quote", "csvlog", "\"unterminated,PANIC\n", false},
		{"unknown format", "stderr", `{"error_severity":"PANIC"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCriticalRecord(c.format, []byte(c.line)); got != c.want {
				t.Fatalf("isCriticalRecord(%s, %q) = %v, want %v",
					c.format, c.line, got, c.want)
			}
		})
	}
}

func TestIsCriticalLog(t *testing.T) {
	cases := []struct {
		level, state string
		want         bool
	}{
		{"PANIC", "", true}, {"panic", "", true}, {"FATAL", "", true},
		{"ERROR", "53300", true}, {"ERROR", "57P03", true},
		{"WARNING", "58P01", true}, {"ERROR", "XX001", true},
		{"ERROR", "XX002", true}, {"ERROR", "XX000", false},
		{"ERROR", "57014", false}, {"ERROR", "5", false},
		{"ERROR", "", false}, {"", "", false}, {"LOG", "00000", false},
	}
	for _, c := range cases {
		if got := isCriticalLog(c.level, c.state); got != c.want {
			t.Errorf("isCriticalLog(%q, %q) = %v, want %v",
				c.level, c.state, got, c.want)
		}
	}
}

// Concurrent readers and the pump share the queue under one mutex: the
// lines read plus the lines dropped equal the lines written.
func TestTailerOverflowConcurrentReadsAccountForEveryLine(t *testing.T) {
	tailer, path := openTestTailer(t)
	const batches, perBatch = 6, 4000
	var wg sync.WaitGroup
	var mu sync.Mutex
	read, dropped := 0, 0
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < batches; i++ {
				n, d := len(tailer.ReadLines()), tailer.TakeDropped()
				mu.Lock()
				read, dropped = read+n, dropped+d
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < batches; i++ {
		appendLogBytes(t, path, noiseStorm(t, perBatch, -1, nil))
		tailer.pump()
	}
	wg.Wait()
	read += len(tailer.ReadLines())
	dropped += tailer.TakeDropped()
	if read+dropped != batches*perBatch {
		t.Fatalf("read %d + dropped %d != written %d", read, dropped,
			batches*perBatch)
	}
}

// State transition: reopening the log (startup or truncation) empties the
// queue, so the critical prefix must go with it; a stale prefix would keep
// the oldest noise lines of the next storm as if they were critical.
func TestTailerReopenResetsCriticalPrefix(t *testing.T) {
	tailer, path := openTestTailer(t)
	panicLine := jsonRecord(t, "PANIC", "", "first crash", 1)
	storm := bytes.Split(bytes.TrimSpace(
		noiseStorm(t, maxTailerQueueSize, 0, panicLine)), []byte{10})
	tailer.mu.Lock()
	tailer.enqueue(storm)
	head := tailer.criticalHead
	tailer.mu.Unlock()
	if head != 1 {
		t.Fatalf("critical prefix after the first storm = %d, want 1", head)
	}
	_ = tailer.file.Close() // openAndSeek replaces the handle
	if err := tailer.openAndSeek(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	noise := bytes.Split(bytes.TrimSpace(
		noiseStorm(t, maxTailerQueueSize+1, -1, nil)), []byte{10})
	tailer.mu.Lock()
	tailer.enqueue(noise)
	tailer.mu.Unlock()
	got := tailer.ReadLines()
	if len(got) != maxTailerQueueSize {
		t.Fatalf("queued = %d, want %d", len(got), maxTailerQueueSize)
	}
	if !bytes.Contains(got[0], []byte(`"pid":101,`)) {
		t.Fatalf("oldest kept line = %s, want noise line 1 (line 0 evicted)", got[0])
	}
}
