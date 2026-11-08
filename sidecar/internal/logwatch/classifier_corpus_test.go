package logwatch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// The corpus under testdata/pg<major>/<format>/ is PostgreSQL's own log
// output, recaptured by scripts/capture_logwatch_corpus.py. Each record is
// preceded by "-- expect=<signal> event=<name> source=<captured|...>".

type corpusRecord struct {
	file   string
	line   int
	expect string
	event  string
	raw    []byte
}

var corpusHeader = regexp.MustCompile(`^-- expect=(\S+) event=(\S+) source=(\S+)$`)

// corpusMajors and the formats each major must cover. jsonlog is PG15+.
var corpusMajors = map[string][]string{
	"pg14": {"csvlog"},
	"pg15": {"csvlog", "jsonlog"},
	"pg16": {"csvlog", "jsonlog"},
	"pg17": {"csvlog", "jsonlog"},
	"pg18": {"csvlog", "jsonlog"},
}

// corpusRequired is every signal the corpus must exercise on every
// major and format, so a recapture that silently loses an event fails.
var corpusRequired = []string{
	"log_panic_server_crash", "log_internal_error", "log_data_corruption",
	"log_wal_segment_removed", "log_replication_slot_inactive",
	"log_wraparound_outage", "log_txid_wraparound_warning",
	"log_multixact_wraparound_warning", "log_multixact_wraparound_outage",
	"log_statement_timeout", "log_temp_file_created",
	"log_checkpoint_too_frequent", "none",
}

func readCorpus(t *testing.T, path string) []corpusRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open corpus %s: %v", path, err)
	}
	defer f.Close()
	var out []corpusRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		n++
		text := strings.TrimRight(sc.Text(), "\r")
		if m := corpusHeader.FindStringSubmatch(text); m != nil {
			out = append(out, corpusRecord{file: path, line: n + 1,
				expect: m[1], event: m[2]})
			continue
		}
		if len(out) == 0 {
			t.Fatalf("%s:%d: record before any expect header", path, n)
		}
		last := &out[len(out)-1]
		if len(last.raw) > 0 {
			last.raw = append(last.raw, '\n')
		}
		last.raw = append(last.raw, text...)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read corpus %s: %v", path, err)
	}
	return out
}

// classifyCorpusRecord runs one record through the production path
// (parse, pre-filter, classify) with a fresh classifier, so dedup from an
// earlier record cannot hide a miss.
func classifyCorpusRecord(t *testing.T, format string, rec corpusRecord) *rca.Signal {
	t.Helper()
	fw := &FileWatcher{
		cfg:        config.LogWatchConfig{Format: format},
		classifier: NewClassifier(ClassifierConfig{}, nopLog),
	}
	_, sig, err := fw.processEvent(rec.raw)
	if err != nil {
		t.Fatalf("%s:%d (%s): parse: %v", rec.file, rec.line, rec.event, err)
	}
	return sig
}

func corpusFiles(t *testing.T) map[string][]string {
	t.Helper()
	files := map[string][]string{}
	for major, formats := range corpusMajors {
		for _, format := range formats {
			dir := filepath.Join("testdata", major, format)
			matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
			if err != nil || len(matches) == 0 {
				t.Fatalf("no corpus in %s (glob err %v)", dir, err)
			}
			files[major+"/"+format] = matches
		}
	}
	return files
}

// CHECK-13: every corpus line tagged expect=<signal> yields that signal.
func TestCorpus_EveryRecordYieldsItsExpectedSignal(t *testing.T) {
	for key, paths := range corpusFiles(t) {
		format := key[strings.Index(key, "/")+1:]
		seen := map[string]bool{}
		for _, path := range paths {
			for _, rec := range readCorpus(t, path) {
				seen[rec.expect] = true
				sig := classifyCorpusRecord(t, format, rec)
				got := "none"
				if sig != nil {
					got = sig.ID
				}
				if got != rec.expect {
					t.Errorf("%s:%d (%s): signal = %s, want %s\nrecord: %s",
						rec.file, rec.line, rec.event, got, rec.expect, rec.raw)
				}
			}
		}
		for _, want := range corpusRequired {
			if !seen[want] {
				t.Errorf("%s: corpus has no record tagged expect=%s", key, want)
			}
		}
	}
}

// CHECK-14: XX000 is an internal error at warning, never corruption.
func TestCorpus_InternalErrorIsWarningNotCorruption(t *testing.T) {
	checked := 0
	for key, paths := range corpusFiles(t) {
		format := key[strings.Index(key, "/")+1:]
		for _, path := range paths {
			for _, rec := range readCorpus(t, path) {
				if rec.expect != "log_internal_error" {
					continue
				}
				sig := classifyCorpusRecord(t, format, rec)
				if sig == nil || sig.ID != "log_internal_error" {
					t.Fatalf("%s:%d: XX000 record gave %v", rec.file, rec.line, sig)
				}
				if sig.Severity != "warning" {
					t.Errorf("%s: severity = %s, want warning", key, sig.Severity)
				}
				if sig.Metrics["sql_state"] != "XX000" {
					t.Errorf("%s: sql_state = %v, want XX000", key, sig.Metrics["sql_state"])
				}
				checked++
			}
		}
	}
	if checked < len(corpusMajors) {
		t.Fatalf("checked %d XX000 records, want at least %d", checked, len(corpusMajors))
	}
}

// The crash, WAL-gap and slot records must carry what an operator needs
// to act: the crashed PID and signal, the missing segment, the slot.
func TestCorpus_ExtractsIdentifiers(t *testing.T) {
	want := map[string][]string{
		"log_panic_server_crash":        {"crashed_pid", "signal", "process_type"},
		"log_wal_segment_removed":       {"wal_segment"},
		"log_replication_slot_inactive": {"slot_name"},
		"log_wraparound_outage":         {"wraparound_database"},
	}
	for key, paths := range corpusFiles(t) {
		format := key[strings.Index(key, "/")+1:]
		for _, path := range paths {
			for _, rec := range readCorpus(t, path) {
				keys, ok := want[rec.expect]
				if !ok {
					continue
				}
				sig := classifyCorpusRecord(t, format, rec)
				if sig == nil {
					t.Fatalf("%s:%d: no signal for %s", rec.file, rec.line, rec.expect)
				}
				for _, k := range keys {
					if v := fmt.Sprint(sig.Metrics[k]); v == "" || v == "<nil>" || v == "0" {
						t.Errorf("%s:%d: %s metric %s = %q", rec.file, rec.line,
							rec.expect, k, v)
					}
				}
			}
		}
	}
}

func TestCorpus_CrashSignalNumbersMatchTheEvent(t *testing.T) {
	wantSignal := map[string]int{"crash_sigkill": 9, "crash_sigsegv": 11}
	for key, paths := range corpusFiles(t) {
		format := key[strings.Index(key, "/")+1:]
		for _, path := range paths {
			for _, rec := range readCorpus(t, path) {
				want, ok := wantSignal[rec.event]
				if !ok {
					continue
				}
				sig := classifyCorpusRecord(t, format, rec)
				if sig == nil {
					t.Fatalf("%s:%d: no crash signal", rec.file, rec.line)
				}
				if got := sig.Metrics["signal"]; got != want {
					t.Errorf("%s:%d: signal metric = %v, want %d", rec.file,
						rec.line, got, want)
				}
			}
		}
	}
}
