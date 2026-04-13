package logwatch

import "time"

// LogEntry is a single parsed PostgreSQL log line.
type LogEntry struct {
	Timestamp   time.Time // log_time, always UTC after parsing
	PID         int       // process_id
	SessionID   string    // session_id
	Database    string    // database_name
	User        string    // user_name
	ErrorLevel  string    // error_severity: LOG, WARNING, ERROR, FATAL, PANIC
	SQLState    string    // sql_state_code (e.g. "40P01" = deadlock)
	Message     string    // message
	Detail      string    // detail
	Hint        string    // hint
	Query       string    // query (if available)
	Application string    // application_name
}

// Truncation constants — not configurable by design.
const (
	MaxRawMessageLen = 500
	MaxQueryLen      = 200
	MaxAffectedPIDs  = 20
)

// Truncate returns s truncated to maxLen with no mid-rune split.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Walk runes to avoid splitting a multi-byte character.
	truncated := 0
	for i := range s {
		if i > maxLen {
			break
		}
		truncated = i
	}
	return s[:truncated]
}
