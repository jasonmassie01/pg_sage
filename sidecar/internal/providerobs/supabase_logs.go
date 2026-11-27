package providerobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

const maxLogRecords = 1000

// ErrLogWindowSaturated means the provider returned a full batch, so the
// window may be truncated; callers narrow the window and retry.
var ErrLogWindowSaturated = errors.New(
	"Supabase log window saturated; narrow window before advancing cursor")

const postgresLogsSelect = `SELECT toString(timestamp) AS timestamp, event_message,
log_attributes['parsed.database_name'] AS database,
log_attributes['parsed.user_name'] AS username,
log_attributes['parsed.error_severity'] AS severity,
log_attributes['parsed.sql_state_code'] AS sql_state,
log_attributes['parsed.application_name'] AS application,
log_attributes['parsed.query'] AS query,
log_attributes['parsed.process_id'] AS pid
FROM logs WHERE source = 'postgres_logs'`

const postgresLogsOrder = ` ORDER BY timestamp ASC LIMIT 1000`

// postgresLogsQuery names the database so a busy neighbour cannot fill the
// row cap. ClickHouse treats a backslash in a string literal as an escape,
// so a name with a backslash, a control character or invalid UTF-8 keeps
// the unfiltered query; the client-side database check applies either way.
func postgresLogsQuery(database string) string {
	if !literalSafe(database) {
		return postgresLogsSelect + postgresLogsOrder
	}
	return postgresLogsSelect + " AND log_attributes['parsed.database_name'] = '" +
		strings.ReplaceAll(database, "'", "''") + "'" + postgresLogsOrder
}

func literalSafe(value string) bool {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\\') {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

type supabaseLog struct {
	Timestamp   string `json:"timestamp"`
	Message     string `json:"event_message"`
	Database    string `json:"database"`
	Username    string `json:"username"`
	Severity    string `json:"severity"`
	SQLState    string `json:"sql_state"`
	Application string `json:"application"`
	Query       string `json:"query"`
	PID         string `json:"pid"`
}

// Logs reads a bounded window. A full batch is an error, never silently truncated success.
// The API rounds windows to minutes; callers must overlap and deduplicate deliveries.
// rejected counts the database's rows skipped for a malformed timestamp or PID.
func (c *Supabase) Logs(ctx context.Context, database string, from, to time.Time) (
	entries []logwatch.LogEntry, rejected int, err error,
) {
	if database == "" || !to.After(from) || to.Sub(from) > 24*time.Hour {
		return nil, 0, errors.New(
			"logs require an exact database and a positive window <=24 hours")
	}
	q := url.Values{"sql": {postgresLogsQuery(database)},
		"iso_timestamp_start": {from.UTC().Format(time.RFC3339Nano)},
		"iso_timestamp_end":   {to.UTC().Format(time.RFC3339Nano)}}
	body, err := c.get(ctx, "logs", q)
	if err != nil {
		return nil, 0, err
	}
	var response struct {
		Result *[]supabaseLog `json:"result"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, 0, errors.New("invalid Supabase logs response")
	}
	if response.Error != "" || response.Result == nil {
		return nil, 0, errors.New("Supabase logs query failed or result is missing")
	}
	if len(*response.Result) >= maxLogRecords {
		return nil, 0, ErrLogWindowSaturated
	}
	entries, rejected = selectLogs(*response.Result, database, from, to)
	return entries, rejected, nil
}

// selectLogs keeps the database's rows inside [from, to). A row with a
// malformed timestamp or PID is skipped and counted, so one bad record
// never holds back the valid ones.
func selectLogs(rows []supabaseLog, database string, from, to time.Time) (
	entries []logwatch.LogEntry, rejected int,
) {
	entries = make([]logwatch.LogEntry, 0, len(rows))
	for _, row := range rows {
		if row.Database != database {
			continue
		}
		at, err := parseLogTime(row.Timestamp)
		if err != nil {
			rejected++
			continue
		}
		if at.Before(from) || !at.Before(to) {
			continue
		}
		pid, ok := parseLogPID(row.PID)
		if !ok {
			rejected++
			continue
		}
		entries = append(entries, logwatch.LogEntry{Timestamp: at, Database: row.Database,
			User: row.Username, ErrorLevel: row.Severity, SQLState: row.SQLState,
			Application: row.Application, Query: row.Query, Message: row.Message, PID: pid})
	}
	return entries, rejected
}

// parseLogPID reads an optional non-negative process ID ("" is 0).
func parseLogPID(value string) (int, bool) {
	if value == "" {
		return 0, true
	}
	pid, err := strconv.Atoi(value)
	return pid, err == nil && pid >= 0
}

func parseLogTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid Supabase log timestamp")
}
