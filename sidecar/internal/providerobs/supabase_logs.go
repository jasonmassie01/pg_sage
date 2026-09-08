package providerobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

const maxLogRecords = 1000

const postgresLogsSQL = `SELECT toString(timestamp) AS timestamp, event_message,
log_attributes['parsed.database_name'] AS database,
log_attributes['parsed.user_name'] AS username,
log_attributes['parsed.error_severity'] AS severity,
log_attributes['parsed.sql_state_code'] AS sql_state,
log_attributes['parsed.application_name'] AS application,
log_attributes['parsed.query'] AS query,
log_attributes['parsed.process_id'] AS pid
FROM logs WHERE source = 'postgres_logs' ORDER BY timestamp ASC LIMIT 1000`

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
func (c *Supabase) Logs(ctx context.Context, database string, from, to time.Time) (
	[]logwatch.LogEntry, error,
) {
	if database == "" || !to.After(from) || to.Sub(from) > 24*time.Hour {
		return nil, errors.New("logs require an exact database and a positive window <=24 hours")
	}
	q := url.Values{"sql": {postgresLogsSQL},
		"iso_timestamp_start": {from.UTC().Format(time.RFC3339Nano)},
		"iso_timestamp_end":   {to.UTC().Format(time.RFC3339Nano)}}
	body, err := c.get(ctx, "logs", q)
	if err != nil {
		return nil, err
	}
	var response struct {
		Result *[]supabaseLog `json:"result"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.New("invalid Supabase logs response")
	}
	if response.Error != "" || response.Result == nil {
		return nil, errors.New("Supabase logs query failed or result is missing")
	}
	if len(*response.Result) >= maxLogRecords {
		return nil, errors.New("Supabase log window saturated; narrow window before advancing cursor")
	}
	return selectLogs(*response.Result, database, from, to)
}

func selectLogs(rows []supabaseLog, database string, from, to time.Time) (
	[]logwatch.LogEntry, error,
) {
	entries := make([]logwatch.LogEntry, 0, len(rows))
	for _, row := range rows {
		if row.Database != database {
			continue
		}
		at, err := parseLogTime(row.Timestamp)
		if err != nil {
			return nil, err
		}
		if at.Before(from) || !at.Before(to) {
			continue
		}
		pid := 0
		if row.PID != "" {
			pid, err = strconv.Atoi(row.PID)
			if err != nil || pid < 0 {
				return nil, errors.New("invalid Supabase log process ID")
			}
		}
		entries = append(entries, logwatch.LogEntry{Timestamp: at, Database: row.Database,
			User: row.Username, ErrorLevel: row.Severity, SQLState: row.SQLState,
			Application: row.Application, Query: row.Query, Message: row.Message, PID: pid})
	}
	return entries, nil
}

func parseLogTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid Supabase log timestamp")
}
