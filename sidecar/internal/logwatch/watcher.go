package logwatch

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"sync"
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

	fw.tailer = NewTailer(
		fw.cfg.LogDirectory, format, pollInterval, maxLineLen, fw.logFn,
	)
	fw.classifier = NewClassifier(ClassifierConfig{
		DedupWindowS:     fw.cfg.DedupWindowS,
		ExcludeApps:      fw.cfg.ExcludeApplications,
		SlowQueryEnabled: fw.cfg.SlowQueryEnabled,
		TempFileMinBytes: int64(fw.cfg.TempFileMinBytes),
		MaxLinesPerCycle: fw.cfg.MaxLinesPerCycle,
	}, fw.logFn)

	if err := fw.tailer.Start(ctx); err != nil {
		return fmt.Errorf("logwatch: start tailer: %w", err)
	}
	fw.log("info", "file watcher started: dir=%s format=%s",
		fw.cfg.LogDirectory, format)
	return nil
}

// Drain reads all new lines from the tailer, parses and classifies
// each one, and returns the resulting signals. It also resets the
// classifier cycle counter and cleans expired dedup entries.
func (fw *FileWatcher) Drain() []*rca.Signal {
	if fw.tailer == nil || fw.classifier == nil {
		return nil
	}
	lines := fw.tailer.ReadLines()

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
		fw.log("warn", "%d log record(s) failed to parse this cycle; first error: %v",
			parseErrors, firstErr)
	}

	fw.classifier.ResetCycle()
	fw.classifier.CleanExpiredDedup()
	return signals
}

// Stop shuts down the tailer.
func (fw *FileWatcher) Stop() {
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
