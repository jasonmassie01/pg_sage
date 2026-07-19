package logwatch

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
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
	for _, line := range lines {
		entry, sig, ok := fw.processEvent(line)
		if ok {
			fw.publishEntry(entry)
		}
		if sig != nil {
			signals = append(signals, sig)
		}
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

func (fw *FileWatcher) processEvent(
	line []byte,
) (LogEntry, *rca.Signal, bool) {
	format := fw.cfg.Format
	if format == "" {
		format = "jsonlog"
	}

	switch format {
	case "jsonlog":
		entry, err := ParseJSONLogLine(line)
		return fw.classifyEvent(entry, err)
	case "csvlog":
		entry, err := fw.parseCSVLine(line)
		return fw.classifyEvent(entry, err)
	default:
		return LogEntry{}, nil, false
	}
}

func (fw *FileWatcher) classifyEvent(
	entry LogEntry, err error,
) (LogEntry, *rca.Signal, bool) {
	if err != nil {
		return LogEntry{}, nil, false
	}
	if !ShouldParseLine(entry.ErrorLevel, entry.Message) {
		return entry, nil, true
	}
	return entry, fw.classifier.Classify(entry), true
}

func (fw *FileWatcher) parseCSVLine(line []byte) (LogEntry, error) {
	reader := csv.NewReader(bytes.NewReader(line))
	reader.FieldsPerRecord = -1 // variable columns across PG versions
	record, err := reader.Read()
	if err != nil {
		if err != io.EOF {
			fw.log("debug", "csv parse error: %v", err)
		}
		return LogEntry{}, err
	}
	return ParseCSVLogLine(record)
}

// log emits a diagnostic message via the configured logFn.
func (fw *FileWatcher) log(level, msg string, args ...any) {
	if fw.logFn != nil {
		fw.logFn(level, msg, args...)
	}
}
