package logwatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// settingsQuery fetches log-relevant settings from PostgreSQL.
const settingsQuery = `SELECT name, setting FROM pg_settings
WHERE name IN ('log_directory', 'data_directory', 'log_destination',
  'logging_collector')`

// DetectLogSettings queries pg_settings to resolve the log directory
// and log format. Returns an error if the log_destination is not
// csvlog or jsonlog, if logging_collector is off, or if the resolved
// directory does not exist; each error names the setting to change.
func DetectLogSettings(
	ctx context.Context, pool *pgxpool.Pool,
) (dir string, format string, err error) {
	settings, err := fetchLogSettings(ctx, pool)
	if err != nil {
		return "", "", fmt.Errorf("logwatch detect: %w", err)
	}

	format, err = resolveFormat(settings["log_destination"])
	if err != nil {
		return "", "", err
	}
	if err := checkCollector(settings["logging_collector"]); err != nil {
		return "", "", err
	}

	dir, err = resolveLogDir(
		settings["log_directory"], settings["data_directory"])
	if err != nil {
		return "", "", err
	}

	if _, statErr := os.Stat(dir); statErr != nil {
		return "", "", fmt.Errorf("logwatch detect: log_directory %s is not "+
			"visible to the sidecar (%v): mount the server's log_directory "+
			"read-only into the sidecar at the same path, or set "+
			"logwatch.log_directory", dir, statErr)
	}
	return dir, format, nil
}

// fetchLogSettings runs the pg_settings query and returns a map of
// name -> setting.
func fetchLogSettings(
	ctx context.Context, pool *pgxpool.Pool,
) (map[string]string, error) {
	rows, err := pool.Query(ctx, settingsQuery)
	if err != nil {
		return nil, fmt.Errorf("query pg_settings: %w", err)
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var name, setting string
		if err := rows.Scan(&name, &setting); err != nil {
			return nil, fmt.Errorf("scan pg_settings row: %w", err)
		}
		m[name] = setting
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pg_settings: %w", err)
	}
	return m, nil
}

// resolveFormat picks the log format from the log_destination value.
// PostgreSQL allows comma-separated destinations (e.g. "csvlog,stderr").
func resolveFormat(logDest string) (string, error) {
	if strings.Contains(logDest, "jsonlog") {
		return "jsonlog", nil
	}
	if strings.Contains(logDest, "csvlog") {
		return "csvlog", nil
	}
	return "", fmt.Errorf("%w: log_destination is %q; pg_sage reads only "+
		"jsonlog (PostgreSQL 15+) or csvlog files: set log_destination = "+
		"'stderr,jsonlog' (or 'stderr,csvlog') with logging_collector = on, "+
		"then reload PostgreSQL", ErrLogSourceUnsupported, logDest)
}

// checkCollector rejects a server whose logging collector is off: csvlog
// and jsonlog files are written only by the collector.
func checkCollector(setting string) error {
	if setting == "on" {
		return nil
	}
	return fmt.Errorf("%w: logging_collector is %q; csvlog and jsonlog files "+
		"are written only with logging_collector = on (a restart is needed)",
		ErrLogSourceUnsupported, setting)
}

// resolveLogDir returns an absolute path for the log directory.
// If log_directory is relative, it is joined with data_directory.
func resolveLogDir(logDir, dataDir string) (string, error) {
	if logDir == "" {
		return "", fmt.Errorf("logwatch detect: log_directory is empty or not " +
			"visible to the pg_sage role: grant it pg_read_all_settings (or " +
			"pg_monitor), or set logwatch.log_directory")
	}
	if filepath.IsAbs(logDir) {
		return logDir, nil
	}
	if dataDir == "" {
		return "", fmt.Errorf(
			"logwatch detect: relative log_directory %q but "+
				"data_directory is empty", logDir)
	}
	return filepath.Join(dataDir, logDir), nil
}
