package logwatch

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
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
}

// NewFileWatcher creates a FileWatcher from the given config.
// Call Start to begin tailing.
func NewFileWatcher(
	cfg config.LogWatchConfig,
	logFn func(string, string, ...any),
) *FileWatcher {
	return &FileWatcher{
		cfg:   cfg,
		logFn: logFn,
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
		sig := fw.processLine(line)
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
}

// processLine parses a single raw line using the configured format
// and classifies it. Returns nil if the line should be skipped.
func (fw *FileWatcher) processLine(line []byte) *rca.Signal {
	format := fw.cfg.Format
	if format == "" {
		format = "jsonlog"
	}

	switch format {
	case "jsonlog":
		return fw.processJSONLine(line)
	case "csvlog":
		return fw.processCSVLine(line)
	default:
		return nil
	}
}

// processJSONLine parses a jsonlog line, pre-filters, and classifies.
func (fw *FileWatcher) processJSONLine(line []byte) *rca.Signal {
	entry, err := ParseJSONLogLine(line)
	if err != nil {
		return nil
	}
	if !ShouldParseLine(entry.ErrorLevel, entry.Message) {
		return nil
	}
	return fw.classifier.Classify(entry)
}

// processCSVLine parses a csvlog line, pre-filters, and classifies.
func (fw *FileWatcher) processCSVLine(line []byte) *rca.Signal {
	reader := csv.NewReader(bytes.NewReader(line))
	reader.FieldsPerRecord = -1 // variable columns across PG versions
	record, err := reader.Read()
	if err != nil {
		if err != io.EOF {
			fw.log("debug", "csv parse error: %v", err)
		}
		return nil
	}
	entry, err := ParseCSVLogLine(record)
	if err != nil {
		return nil
	}
	if !ShouldParseLine(entry.ErrorLevel, entry.Message) {
		return nil
	}
	return fw.classifier.Classify(entry)
}

// log emits a diagnostic message via the configured logFn.
func (fw *FileWatcher) log(level, msg string, args ...any) {
	if fw.logFn != nil {
		fw.logFn(level, msg, args...)
	}
}
