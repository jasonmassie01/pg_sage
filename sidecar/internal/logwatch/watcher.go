package logwatch

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// LogSource is the interface that the RCA engine uses to consume
// parsed log signals from any log backend (file, syslog, etc.).
type LogSource interface {
	Start(ctx context.Context) error
	Drain() []*rca.Signal
	Stop()
}

// FileWatcher ties a Tailer, parser, and Classifier together to
// implement LogSource for PostgreSQL log files on disk.
type FileWatcher struct {
	cfg        config.LogWatchConfig
	logFn      func(string, string, ...any)
	tailer     *Tailer
	classifier *Classifier
	entryMu    sync.Mutex
	entries    map[string]*entryBuffer
	drops      dropCounters
	// stateMu guards tailer and started against Status readers.
	stateMu     sync.RWMutex
	started     bool
	lastLine    atomic.Int64 // unix nanos of the last record read; 0 = none
	parseErrors atomic.Uint64
	// deadlockText is rca.deadlock_statement_text for the classifier.
	deadlockText string
}

// NewFileWatcher creates a FileWatcher from the given config.
// Call Start to begin tailing.
func NewFileWatcher(
	cfg config.LogWatchConfig,
	logFn func(string, string, ...any),
) *FileWatcher {
	return &FileWatcher{
		cfg:     cfg,
		logFn:   logFn,
		entries: make(map[string]*entryBuffer),
	}
}

// WithDeadlockStatementText sets rca.deadlock_statement_text for the
// classifier Start builds (rca-07b). Call it before Start.
func (fw *FileWatcher) WithDeadlockStatementText(mode string) *FileWatcher {
	fw.deadlockText = mode
	return fw
}

// StartWithConfig replaces the config, then starts: a watcher created
// before its source was detected starts on the detected directory and
// format. Call it only while no Drain runs (before the drain loop).
func (fw *FileWatcher) StartWithConfig(
	ctx context.Context, cfg config.LogWatchConfig,
) error {
	fw.cfg = cfg
	return fw.Start(ctx)
}

// Start creates the Tailer and Classifier from config, then starts
// the tailer's background poll loop.
func (fw *FileWatcher) Start(ctx context.Context) error {
	format := fw.cfg.Format
	if format == "" {
		format = "jsonlog"
	}
	pollInterval := time.Duration(fw.cfg.PollIntervalMs) * time.Millisecond
	if pollInterval == 0 {
		pollInterval = 1000 * time.Millisecond
	}
	maxLineLen := fw.cfg.MaxLineLenBytes
	if maxLineLen == 0 {
		maxLineLen = 65536
	}

	tailer := NewTailer(
		fw.cfg.LogDirectory, format, pollInterval, maxLineLen, fw.logFn,
	)
	fw.stateMu.Lock()
	fw.tailer, fw.started = tailer, false
	fw.stateMu.Unlock()
	fw.classifier = NewClassifier(ClassifierConfig{
		DedupWindowS:     fw.cfg.DedupWindowS,
		ExcludeApps:      fw.cfg.ExcludeApplications,
		SlowQueryEnabled: fw.cfg.SlowQueryEnabled,
		TempFileMinBytes: int64(fw.cfg.TempFileMinBytes),
		MaxLinesPerCycle: fw.cfg.MaxLinesPerCycle,

		DeadlockStatementText: fw.deadlockText,
	}, fw.logFn)

	if err := tailer.Start(ctx); err != nil {
		return fmt.Errorf("logwatch: start tailer: %w", err)
	}
	fw.stateMu.Lock()
	fw.started = true
	fw.stateMu.Unlock()
	fw.log("info", "file watcher started: dir=%s format=%s",
		fw.cfg.LogDirectory, format)
	return nil
}

// Drain reads all new lines from the tailer, parses and classifies
// each one, and returns the resulting signals. It also resets the
// classifier cycle counter, reports the repeats the dedup windows counted
// (one follow-up per window) and cleans expired dedup entries. When lines
// were lost since the last drain (queue overflow or the per-cycle cap) it
// counts them and appends a log_watcher_overloaded signal (rca-06).
func (fw *FileWatcher) Drain() []*rca.Signal {
	if fw.tailer == nil || fw.classifier == nil {
		return nil
	}
	lines := fw.tailer.ReadLines()
	if len(lines) > 0 {
		fw.lastLine.Store(time.Now().UnixNano())
	}
	queueDropped := fw.tailer.TakeDropped()
	fw.countOversized(fw.tailer.TakeOversized())
	signals := fw.classifyLines(lines)
	// After the drain's own signals, so the counted follow-up is the last
	// of its id (rca-07).
	signals = append(signals, fw.classifier.FollowUps()...)
	capped := fw.classifier.ResetCycle()
	fw.classifier.CleanExpiredDedup()
	fw.drops.add(queueDropped, capped)
	if sig := overloadSignal(queueDropped, capped, time.Now()); sig != nil {
		fw.log("warn", "log watcher overloaded: dropped %d log lines "+
			"(%d queue overflow, %d over logwatch.max_lines_per_cycle); "+
			"critical lines were kept", queueDropped+capped, queueDropped, capped)
		signals = append(signals, sig)
	}
	return signals
}

// countOversized counts and reports the records the tailer dropped for
// their size: a plan over PlanRecordMaxBytes, or any other record over
// logwatch.max_line_len_bytes (rca-04).
func (fw *FileWatcher) countOversized(n int) {
	if n <= 0 {
		return
	}
	fw.drops.addOversized(n)
	fw.log("warn", "log watcher dropped %d oversized log records (over "+
		"logwatch.max_line_len_bytes, or %d bytes for an auto_explain plan)", n,
		PlanRecordMaxBytes)
}

// DroppedLines reports the lines this watcher lost since it started.
func (fw *FileWatcher) DroppedLines() DropCounts {
	return fw.drops.snapshot()
}

// classifyLines parses, publishes and classifies one drain's records.
func (fw *FileWatcher) classifyLines(lines [][]byte) []*rca.Signal {
	var signals []*rca.Signal
	parseErrors := 0
	var firstErr error
	for _, line := range lines {
		entry, sig, err := fw.processEvent(line)
		if err != nil {
			parseErrors++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fw.publishEntry(entry)
		if sig != nil {
			signals = append(signals, sig)
		}
	}
	if parseErrors > 0 {
		fw.parseErrors.Add(uint64(parseErrors))
		fw.log("warn", "%d log record(s) failed to parse this cycle; first error: %v",
			parseErrors, firstErr)
	}
	return signals
}

// Stop shuts down the tailer.
func (fw *FileWatcher) Stop() {
	fw.stateMu.Lock()
	fw.started = false
	fw.stateMu.Unlock()
	if fw.tailer != nil {
		fw.tailer.Stop()
		fw.log("info", "file watcher stopped")
	}
	fw.clearEntryBuffers()
}

// processEvent parses one log record and classifies it. A parse error is
// returned to the caller so dropped records are counted and reported.
func (fw *FileWatcher) processEvent(line []byte) (LogEntry, *rca.Signal, error) {
	format := fw.cfg.Format
	if format == "" {
		format = "jsonlog"
	}

	var entry LogEntry
	var err error
	switch format {
	case "jsonlog":
		entry, err = ParseJSONLogLine(line)
	case "csvlog":
		entry, err = parseCSVRecord(line)
	default:
		return LogEntry{}, nil, fmt.Errorf("logwatch: unsupported format %q", format)
	}
	if err != nil {
		return LogEntry{}, nil, err
	}
	if !ShouldParseLine(entry.ErrorLevel, entry.Message) {
		return entry, nil, nil
	}
	return entry, fw.classifier.Classify(entry), nil
}

// parseCSVRecord parses one complete csvlog record (which may span
// several physical lines inside quoted fields).
func parseCSVRecord(record []byte) (LogEntry, error) {
	reader := csv.NewReader(bytes.NewReader(record))
	reader.FieldsPerRecord = -1 // variable columns across PG versions
	fields, err := reader.Read()
	if err != nil {
		return LogEntry{}, fmt.Errorf("logwatch: csv parse: %w", err)
	}
	return ParseCSVLogLine(fields)
}

// log emits a diagnostic message via the configured logFn.
func (fw *FileWatcher) log(level, msg string, args ...any) {
	if fw.logFn != nil {
		fw.logFn(level, msg, args...)
	}
}
