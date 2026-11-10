package logwatch

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// signalSettingsQuery reads the settings that decide whether optional log
// signals can fire. It is a read of pg_settings only.
const signalSettingsQuery = `SELECT name, setting FROM pg_settings
WHERE name IN ('log_temp_files', 'log_min_duration_statement')`

// DetectSignalHints reads the server settings that feed optional log
// signals and returns a hint for each one that keeps its signal silent.
func DetectSignalHints(
	ctx context.Context, pool *pgxpool.Pool, slowQuery bool,
) ([]string, error) {
	rows, err := pool.Query(ctx, signalSettingsQuery)
	if err != nil {
		return nil, fmt.Errorf("logwatch detect: read signal settings: %w", err)
	}
	defer rows.Close()
	settings := map[string]string{}
	for rows.Next() {
		var name, setting string
		if err := rows.Scan(&name, &setting); err != nil {
			return nil, fmt.Errorf("logwatch detect: scan signal setting: %w", err)
		}
		settings[name] = setting
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("logwatch detect: read signal settings: %w", err)
	}
	return SignalSettingHints(settings, slowQuery), nil
}

// SignalSettingHints names each setting that keeps an optional signal from
// firing: log_temp_files = -1 logs no temporary file, and, when slow-query
// parsing is on, log_min_duration_statement = -1 logs no duration. An
// unread setting gives no hint.
func SignalSettingHints(settings map[string]string, slowQuery bool) []string {
	var hints []string
	if settings["log_temp_files"] == "-1" {
		hints = append(hints, "log_temp_files is -1, so no temporary file is "+
			"logged and log_temp_file_created never fires; set log_temp_files = "+
			"'10MB' (or 0 for every file)")
	}
	if slowQuery && settings["log_min_duration_statement"] == "-1" {
		hints = append(hints, "log_min_duration_statement is -1, so no duration "+
			"is logged and log_slow_query never fires; set it, for example to '1s'")
	}
	return hints
}
