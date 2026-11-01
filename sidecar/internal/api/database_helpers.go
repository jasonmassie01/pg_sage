package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// ConnectionTestResult holds the result of a database
// connection test.
type ConnectionTestResult struct {
	Status     string   `json:"status"`
	PGVersion  string   `json:"pg_version,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Error      string   `json:"error,omitempty"`
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
	return testFromConnStringAtIP(ctx, connStr, "")
}

func testFromConnStringAtIP(
	ctx context.Context, connStr, pinnedIP string,
) *ConnectionTestResult {
	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cfg, err := sageConnectionConfig(testCtx, connStr, pinnedIP, databricksauth.ConfigureConnection)
	if err != nil {
		log.Printf("test-connection configuration failed: %v", err)
		return &ConnectionTestResult{Status: "error", Error: "connection failed"}
	}
	conn, err := pgx.ConnectConfig(testCtx, cfg)
	if err != nil {
		log.Printf("test-connection (by connstr) failed: %v", err)
		return &ConnectionTestResult{
			Status: "error",
			Error:  "connection failed",
		}
	}
	defer func() { _ = conn.Close(testCtx) }()

	version, err := queryPGVersion(testCtx, conn)
	if err != nil {
		log.Printf("test-connection (by connstr) version "+
			"query failed: %v", err)
		return &ConnectionTestResult{
			Status: "error",
			Error:  "version query failed",
		}
	}
	return &ConnectionTestResult{
		Status:     "ok",
		PGVersion:  version,
		Extensions: queryExtensions(testCtx, conn),
	}
}

// sageConnectionConfig preserves the endpoint's identity while pinning preview
// connections to the address approved by the SSRF check. Sessions are tagged
// like the monitoring pools and use their configured authentication mode.
func sageConnectionConfig(ctx context.Context, connStr, pinnedIP string,
	authorize func(context.Context, *pgx.ConnConfig) error,
) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}
	if pinnedIP != "" {
		if net.ParseIP(pinnedIP) == nil {
			return nil, fmt.Errorf("invalid pinned database IP")
		}
		if _, status := resolveSafeHost(pinnedIP); status != hostAllowed {
			return nil, fmt.Errorf("unsafe pinned database IP")
		}
		host := cfg.Host
		cfg.LookupFunc = func(_ context.Context, requested string) ([]string, error) {
			if !strings.EqualFold(requested, host) {
				return nil, fmt.Errorf("database lookup differs from the pinned host")
			}
			return []string{pinnedIP}, nil
		}
	}
	// Use the same configured endpoint mapping as monitoring pools, including
	// replacing any bootstrap password when native OAuth is selected.
	if err := authorize(ctx, cfg); err != nil {
		return nil, err
	}
	selfmonitor.ConfigureConn(cfg)
	return cfg, nil
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
