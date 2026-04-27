package migration

import (
	"context"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/rca"
)

// LogDetector processes PostgreSQL log entries and identifies DDL
// statements for risk assessment. It integrates with the logwatch
// package as a subscriber.
type LogDetector struct {
	advisor *Advisor
	logFn   func(string, string, ...any)
}

// NewLogDetector creates a LogDetector backed by the given Advisor.
func NewLogDetector(
	advisor *Advisor,
	logFn func(string, string, ...any),
) *LogDetector {
	return &LogDetector{advisor: advisor, logFn: logFn}
}

// ProcessLogEntry checks if a log entry contains DDL and, if so,
// runs it through the advisor for risk assessment. Returns nil if
// the entry is not DDL or the risk is below threshold.
func (ld *LogDetector) ProcessLogEntry(
	entry logwatch.LogEntry,
) *rca.Incident {
	sql := extractDDLFromEntry(entry)
	if sql == "" {
		return nil
	}

	ctx := context.Background()
	inc, err := ld.advisor.Analyze(ctx, sql)
	if err != nil {
		ld.logFn("warn",
			"migration: log entry analysis failed: %v", err)
		return nil
	}
	return inc
}

// extractDDLFromEntry returns the DDL SQL from a log entry, or ""
// if the entry does not contain a DDL statement.
func extractDDLFromEntry(entry logwatch.LogEntry) string {
	// Check the Query field first (most specific).
	if entry.Query != "" && isDDLKeyword(entry.Query) {
		return entry.Query
	}
	// Fall back to the Message field.
	if isDDLKeyword(entry.Message) {
		return entry.Message
	}
	return ""
}
