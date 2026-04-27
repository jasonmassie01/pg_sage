package logwatch

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// jsonLogRaw maps the PostgreSQL jsonlog format (PG15+) for unmarshalling.
type jsonLogRaw struct {
	Timestamp   string `json:"timestamp"`
	PID         int    `json:"pid"`
	SessionID   string `json:"session_id"`
	Database    string `json:"database_name"`
	User        string `json:"user_name"`
	Severity    string `json:"error_severity"`
	StateCode   string `json:"state_code"`
	Message     string `json:"message"`
	Detail      string `json:"detail"`
	Hint        string `json:"hint"`
	Query       string `json:"query"`
	Application string `json:"application_name"`
}

// Timestamp formats used by PostgreSQL log output.
var (
	// csvlog: "2023-10-15 14:30:00.123 UTC" or "2023-10-15 14:30:00.123+00"
	csvTimestampFormats = []string{
		"2006-01-02 15:04:05.999 MST",
		"2006-01-02 15:04:05.999-07",
		"2006-01-02 15:04:05.999+07",
		"2006-01-02 15:04:05.999-07:00",
		"2006-01-02 15:04:05.999+07:00",
	}

	// jsonlog: ISO 8601 "2023-10-15T14:30:00.123+00:00"
	jsonTimestampFormat = time.RFC3339Nano
)

// Keywords that make a LOG-severity line worth parsing.
var logKeywords = []string{
	"deadlock",
	"temporary file",
	"checkpoint",
	"archive",
	"autovacuum",
	"replication slot",
	"vacuum",
	"terminated by signal",
	"duration:",
}

// ParseJSONLogLine parses a single PostgreSQL jsonlog line (PG15+).
// Non-UTF-8 bytes are replaced with U+FFFD before unmarshalling.
func ParseJSONLogLine(line []byte) (LogEntry, error) {
	sanitized := strings.ToValidUTF8(string(line), "\uFFFD")

	var raw jsonLogRaw
	if err := json.Unmarshal([]byte(sanitized), &raw); err != nil {
		return LogEntry{}, fmt.Errorf("logwatch: json unmarshal: %w", err)
	}

	ts, err := parseJSONTimestamp(raw.Timestamp)
	if err != nil {
		return LogEntry{}, fmt.Errorf("logwatch: parse timestamp %q: %w", raw.Timestamp, err)
	}

	return LogEntry{
		Timestamp:   ts,
		PID:         raw.PID,
		SessionID:   raw.SessionID,
		Database:    raw.Database,
		User:        raw.User,
		ErrorLevel:  raw.Severity,
		SQLState:    raw.StateCode,
		Message:     raw.Message,
		Detail:      raw.Detail,
		Hint:        raw.Hint,
		Query:       raw.Query,
		Application: raw.Application,
	}, nil
}

// parseJSONTimestamp parses an ISO 8601 timestamp and converts to UTC.
func parseJSONTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(jsonTimestampFormat, s)
	if err != nil {
		// Fall back: some PG builds emit slightly different formats.
		t, err = time.Parse("2006-01-02T15:04:05.999-07:00", s)
		if err != nil {
			return time.Time{}, err
		}
	}
	return t.UTC(), nil
}

const (
	csvMinColumns  = 23 // PG <14
	csvExtColumns  = 26 // PG14+ (adds backend_type, leader_pid, query_id)
)

// CSV column indices.
const (
	csvColTime        = 0
	csvColUser        = 1
	csvColDatabase    = 2
	csvColPID         = 3
	csvColSessionID   = 5
	csvColSeverity    = 11
	csvColSQLState    = 12
	csvColMessage     = 13
	csvColDetail      = 14
	csvColHint        = 15
	csvColQuery       = 19
	csvColApplication = 22
)

// ParseCSVLogLine parses a pre-split CSV record from pg_csvlog.
// The caller is responsible for splitting via encoding/csv.
func ParseCSVLogLine(record []string) (LogEntry, error) {
	if len(record) < csvMinColumns {
		return LogEntry{}, fmt.Errorf(
			"logwatch: csv record has %d columns, need at least %d",
			len(record), csvMinColumns,
		)
	}

	pid, err := strconv.Atoi(record[csvColPID])
	if err != nil {
		return LogEntry{}, fmt.Errorf("logwatch: parse pid %q: %w", record[csvColPID], err)
	}

	ts, err := parseCSVTimestamp(record[csvColTime])
	if err != nil {
		return LogEntry{}, fmt.Errorf(
			"logwatch: parse timestamp %q: %w", record[csvColTime], err,
		)
	}

	return LogEntry{
		Timestamp:   ts,
		PID:         pid,
		SessionID:   record[csvColSessionID],
		Database:    record[csvColDatabase],
		User:        record[csvColUser],
		ErrorLevel:  record[csvColSeverity],
		SQLState:    record[csvColSQLState],
		Message:     record[csvColMessage],
		Detail:      record[csvColDetail],
		Hint:        record[csvColHint],
		Query:       record[csvColQuery],
		Application: record[csvColApplication],
	}, nil
}

// parseCSVTimestamp tries each known csvlog timestamp format and returns UTC.
func parseCSVTimestamp(s string) (time.Time, error) {
	for _, layout := range csvTimestampFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("no matching format for %q", s)
}

// ShouldParseLine is a pre-filter that decides whether a log line is
// worth full parsing based on severity and message content.
func ShouldParseLine(severity string, message string) bool {
	sev := strings.ToUpper(severity)
	switch sev {
	case "WARNING", "ERROR", "FATAL", "PANIC":
		return true
	case "LOG":
		return containsLogKeyword(message)
	default:
		return false
	}
}

// containsLogKeyword checks if msg contains any keyword of interest
// (case-insensitive).
func containsLogKeyword(msg string) bool {
	lower := strings.ToLower(msg)
	for _, kw := range logKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
