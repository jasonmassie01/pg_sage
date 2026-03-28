package api

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectionTestResult holds the result of a database
// connection test.
type ConnectionTestResult struct {
	Status     string   `json:"status"`
	PGVersion  string   `json:"pg_version,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// testDatabaseConnection attempts to connect to a database
// and returns diagnostics about PG version and extensions.
func testDatabaseConnection(
	ctx context.Context,
	host string, port int,
	database, username, password, sslmode string,
) (*ConnectionTestResult, error) {
	connStr := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		username, password, host, port, database, sslmode,
	)

	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(testCtx, connStr)
	if err != nil {
		return &ConnectionTestResult{
			Status: "error",
			Error:  fmt.Sprintf("connection failed: %v", err),
		}, nil
	}
	defer conn.Close(testCtx)

	version, err := queryPGVersion(testCtx, conn)
	if err != nil {
		return &ConnectionTestResult{
			Status: "error",
			Error:  fmt.Sprintf("version query failed: %v", err),
		}, nil
	}

	extensions := queryExtensions(testCtx, conn)

	return &ConnectionTestResult{
		Status:     "ok",
		PGVersion:  version,
		Extensions: extensions,
	}, nil
}

// queryPGVersion runs SELECT version() and extracts
// the short version string.
func queryPGVersion(
	ctx context.Context, conn *pgx.Conn,
) (string, error) {
	var version string
	err := conn.QueryRow(ctx, "SELECT version()").Scan(&version)
	if err != nil {
		return "", fmt.Errorf("querying version: %w", err)
	}
	return version, nil
}

// testFromConnString tests a connection using a full
// connection string (as returned by DatabaseStore).
func testFromConnString(
	ctx context.Context, connStr string,
) *ConnectionTestResult {
	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(testCtx, connStr)
	if err != nil {
		return &ConnectionTestResult{
			Status: "error",
			Error:  fmt.Sprintf("connection failed: %v", err),
		}
	}
	defer conn.Close(testCtx)

	version, err := queryPGVersion(testCtx, conn)
	if err != nil {
		return &ConnectionTestResult{
			Status: "error",
			Error:  fmt.Sprintf("version query failed: %v", err),
		}
	}
	return &ConnectionTestResult{
		Status:     "ok",
		PGVersion:  version,
		Extensions: queryExtensions(testCtx, conn),
	}
}

// queryExtensions checks for pg_stat_statements and
// auto_explain extensions.
func queryExtensions(
	ctx context.Context, conn *pgx.Conn,
) []string {
	var extensions []string
	names := []string{"pg_stat_statements", "auto_explain"}
	for _, name := range names {
		var found int
		err := conn.QueryRow(ctx,
			"SELECT 1 FROM pg_extension WHERE extname = $1",
			name,
		).Scan(&found)
		if err == nil {
			extensions = append(extensions, name)
		}
	}
	if extensions == nil {
		extensions = []string{}
	}
	return extensions
}
