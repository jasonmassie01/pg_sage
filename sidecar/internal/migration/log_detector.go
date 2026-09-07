package migration

import (
	"context"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/rca"
)

// LogDetector processes PostgreSQL log entries and identifies DDL
// statements for risk assessment. It integrates with the logwatch
// package as a subscriber.
type LogDetector struct {
	advisor     *Advisor
	logFn       func(string, string, ...any)
	findingSink FindingSink
}

// NewLogDetector creates a LogDetector backed by the given Advisor.
func NewLogDetector(
	advisor *Advisor,
	logFn func(string, string, ...any),
) *LogDetector {
	return &LogDetector{advisor: advisor, logFn: logFn}
}

// WithFindingSink persists log-detected migration findings through the same
// deduplicating store used by activity polling.
func (ld *LogDetector) WithFindingSink(sink FindingSink) *LogDetector {
	ld.findingSink = sink
	return ld
}

// ProcessLogEntry checks if a log entry contains DDL and, if so,
// runs it through the advisor for risk assessment. Returns nil if
// the entry is not DDL or the risk is below threshold.
func (ld *LogDetector) ProcessLogEntry(
	entry logwatch.LogEntry,
) *rca.Incident {
	return ld.processLogEntry(context.Background(), entry)
}

func (ld *LogDetector) processLogEntry(
	ctx context.Context, entry logwatch.LogEntry,
) *rca.Incident {
	if !ld.enabled() {
		return nil
	}
	sql := extractDDLFromEntry(entry)
	if sql == "" {
		return nil
	}

	inc, err := ld.advisor.Analyze(ctx, sql)
	if err != nil {
		ld.log("warn", "migration: log entry analysis failed: %v", err)
		return nil
	}
	ld.persistFinding(ctx, entry.PID, sql, inc)
	return inc
}

func (ld *LogDetector) enabled() bool {
	return ld != nil && ld.advisor != nil && ld.advisor.cfg != nil &&
		ld.advisor.cfg.Enabled && ld.advisor.cfg.LogDetection
}

func (ld *LogDetector) persistFinding(
	ctx context.Context, pid int, sql string, inc *rca.Incident,
) {
	if ld.findingSink == nil {
		return
	}
	finding, ok := FindingFromIncident(pid, sql, inc)
	if !ok {
		return
	}
	if _, err := ld.findingSink.UpsertMigrationSafetyFinding(
		ctx, finding,
	); err != nil {
		ld.log("warn", "migration: persist log finding failed: %v", err)
	}
}

// LogEntrySource is a bounded parsed-entry subscription backed by logwatch.
type LogEntrySource interface {
	Drain() []logwatch.LogEntry
	Stop()
}

// Run drains parsed entries without reading the PostgreSQL log file itself.
func (ld *LogDetector) Run(
	ctx context.Context, source LogEntrySource, interval time.Duration,
) {
	if !ld.enabled() || source == nil {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer source.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ld.drain(ctx, source)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (ld *LogDetector) drain(ctx context.Context, source LogEntrySource) {
	for _, entry := range source.Drain() {
		if inc := ld.processLogEntry(ctx, entry); inc != nil {
			ld.log("warn", "migration: DDL risk detected from log: %s (score=%.2f)",
				inc.RootCause, inc.Confidence)
		}
	}
}

func (ld *LogDetector) log(level, msg string, args ...any) {
	if ld.logFn != nil {
		ld.logFn(level, msg, args...)
	}
}

// extractDDLFromEntry returns the DDL SQL from a log entry, or ""
// if the entry does not contain a DDL statement.
func extractDDLFromEntry(entry logwatch.LogEntry) string {
	// Check the Query field first (most specific).
	if query := normalizeLoggedStatement(entry.Query); isDDLKeyword(query) {
		return query
	}
	// Fall back to the Message field.
	if message := normalizeLoggedStatement(entry.Message); isDDLKeyword(message) {
		return message
	}
	return ""
}

func normalizeLoggedStatement(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len("statement:") &&
		strings.EqualFold(value[:len("statement:")], "statement:") {
		return strings.TrimSpace(value[len("statement:"):])
	}
	return value
}
