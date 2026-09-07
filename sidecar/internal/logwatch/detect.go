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
WHERE name IN ('log_directory', 'data_directory', 'log_destination')`

// DetectLogSettings queries pg_settings to resolve the log directory
// and log format. Returns an error if the log_destination is not
// csvlog or jsonlog, or if the resolved directory does not exist.
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

	dir, err = resolveLogDir(
		settings["log_directory"], settings["data_directory"])
	if err != nil {
		return "", "", err
	}

	if _, statErr := os.Stat(dir); statErr != nil {
		return "", "", fmt.Errorf(
			"logwatch detect: directory does not exist: %s", dir)
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
	return "", fmt.Errorf(
		"logwatch detect: unsupported log_destination %q "+
			"(need jsonlog or csvlog)", logDest)
}

// resolveLogDir returns an absolute path for the log directory.
// If log_directory is relative, it is joined with data_directory.
func resolveLogDir(logDir, dataDir string) (string, error) {
	if logDir == "" {
		return "", fmt.Errorf(
			"logwatch detect: log_directory is empty")
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
