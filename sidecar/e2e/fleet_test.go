//go:build e2e

// Package e2e — fleet mode E2E test. Builds the binary, starts it
// in fleet mode with two test databases, exercises the REST API
// and Prometheus endpoints, and verifies clean shutdown.
//
// Run with: go test -tags=e2e -count=1 -timeout 180s ./e2e/
//
// Prerequisites:
//   - PostgreSQL running on localhost:5432
//   - Role "postgres" with password "postgres"
//     (or set SAGE_DATABASE_URL)
//   - Go toolchain available for building the binary
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	fleetDB1 = "sage_fleet_test_1"
	fleetDB2 = "sage_fleet_test_2"
)

// adminDSN returns the admin Postgres DSN pointing at the
// "postgres" database, suitable for CREATE/DROP DATABASE.
func adminDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
}

// createTestDB creates a database if it does not exist.
func createTestDB(
	t *testing.T, pool *pgxpool.Pool, name string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 15*time.Second,
	)
	defer cancel()

	// Drop first for a clean slate.
	_, _ = pool.Exec(ctx, fmt.Sprintf(
		"DROP DATABASE IF EXISTS %s", name,
	))
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE DATABASE %s", name,
	))
	if err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", name, err)
	}
}

// dropTestDB drops a database, terminating active connections.
func dropTestDB(
	t *testing.T, pool *pgxpool.Pool, name string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 15*time.Second,
	)
	defer cancel()

	// Terminate backends connected to the target database.
	_, _ = pool.Exec(ctx, fmt.Sprintf(
		"SELECT pg_terminate_backend(pid) "+
			"FROM pg_stat_activity WHERE datname='%s'",
		name,
	))
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"DROP DATABASE IF EXISTS %s", name,
	))
	if err != nil {
		t.Logf("DROP DATABASE %s: %v", name, err)
	}
}

// dbDSN builds a DSN targeting a specific database name on
// localhost using the default postgres credentials.
func dbDSN(dbName string) string {
	return fmt.Sprintf(
		"postgres://postgres:postgres@localhost:5432/%s"+
			"?sslmode=disable",
		dbName,
	)
}

// writeFleetConfig writes a fleet-mode YAML config file that
// monitors two databases.
func writeFleetConfig(
	t *testing.T,
	apiPort, promPort int,
	db1, db2 string,
) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet-config.yaml")

	yaml := fmt.Sprintf(`mode: fleet

databases:
  - name: %s
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    database: %s
    sslmode: disable
    max_connections: 3
  - name: %s
    host: localhost
    port: 5432
    user: postgres
    password: postgres
    database: %s
    sslmode: disable
    max_connections: 3

collector:
  interval_seconds: 5
  batch_size: 100
  max_queries: 200

analyzer:
  interval_seconds: 10
  slow_query_threshold_ms: 500
  seq_scan_min_rows: 10000
  unused_index_window_days: 7
  table_bloat_dead_tuple_pct: 10

trust:
  level: observation

llm:
  enabled: false

prometheus:
  listen_addr: "127.0.0.1:%d"

api:
  listen_addr: "127.0.0.1:%d"
`, db1, db1, db2, db2, promPort, apiPort)

	err := os.WriteFile(path, []byte(yaml), 0644)
	if err != nil {
		t.Fatalf("writeFleetConfig: %v", err)
	}
	return path
}

// startFleetBinary launches pg_sage_sidecar in fleet mode.
func startFleetBinary(
	t *testing.T, env *testEnv,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(
		context.Background(),
	)
	env.cancel = cancel

	cmd := exec.CommandContext(ctx,
		env.binaryPath,
		"--config", env.configPath,
	)
	cmd.Env = append(os.Environ(),
		"SAGE_MODE=fleet",
		"SAGE_DATABASE_URL=",
		"SAGE_LLM_API_KEY=",
		"SAGE_META_DB=",
	)

	env.stdout = &syncBuffer{}
	env.stderr = &syncBuffer{}
	cmd.Stdout = env.stdout
	cmd.Stderr = env.stderr
	env.cmd = cmd

	if err := cmd.Start(); err != nil {
		t.Fatalf("startFleetBinary: %v", err)
	}
}

// clearSageSchema drops sage-managed rows in the given DB so
// the binary bootstraps a fresh admin on startup.
func clearSageSchema(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Logf(
			"clearSageSchema: connect %s: %v (continuing)",
			dsn, err,
		)
		return
	}
	defer pool.Close()

	for _, tbl := range []string{
		"sage.sessions",
		"sage.config_audit",
		"sage.config",
	} {
		_, err := pool.Exec(ctx, "DELETE FROM "+tbl)
		if err != nil {
			t.Logf("clearSageSchema: DELETE %s: %v", tbl, err)
		}
	}
	_, err = pool.Exec(ctx, "DELETE FROM sage.users")
	if err != nil {
		t.Logf("clearSageSchema: DELETE sage.users: %v", err)
	}
}

// TestFleet is the fleet-mode E2E test. It creates two test
// databases, builds the binary, starts it in fleet mode, then
// exercises the REST API endpoints.
func TestFleet(t *testing.T) {
	dsn := adminDSN(t)
	if !pgAvailable(t, dsn) {
		t.Skip(
			"PostgreSQL not reachable, skipping fleet E2E",
		)
	}

	// --- Create test databases ---
	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer adminPool.Close()

	createTestDB(t, adminPool, fleetDB1)
	createTestDB(t, adminPool, fleetDB2)
	t.Cleanup(func() {
		dropTestDB(t, adminPool, fleetDB1)
		dropTestDB(t, adminPool, fleetDB2)
	})

	// The binary bootstraps the sage schema in the first DB.
	// Clear sage.users + FK deps so admin bootstrap fires.
	// The schema may not exist yet (fresh DB), so errors are
	// logged but not fatal.
	clearSageSchema(t, dbDSN(fleetDB1))

	// --- Build binary and start ---
	binary := buildBinary(t)
	apiPort := freePort(t)
	promPort := freePort(t)
	configPath := writeFleetConfig(
		t, apiPort, promPort, fleetDB1, fleetDB2,
	)

	env := &testEnv{
		binaryPath: binary,
		apiPort:    apiPort,
		promPort:   promPort,
		apiBase: fmt.Sprintf(
			"http://127.0.0.1:%d", apiPort,
		),
		promBase: fmt.Sprintf(
			"http://127.0.0.1:%d", promPort,
		),
		configPath: configPath,
	}

	startFleetBinary(t, env)
	t.Cleanup(func() {
		stopBinary(t, env)
	})

	waitReady(t, env)

	// --- Fleet API Endpoint Tests ---

	t.Run("databases_returns_both", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/databases",
		)
		assertStatusOK(t, "databases", code)
		assertJSON(t, "databases", body)
		assertContains(
			t, "databases has db1", body, fleetDB1,
		)
		assertContains(
			t, "databases has db2", body, fleetDB2,
		)
	})

	t.Run("findings_filter_db1", func(t *testing.T) {
		url := fmt.Sprintf(
			"%s/api/v1/findings?database=%s",
			env.apiBase, fleetDB1,
		)
		code, body := httpGet(t, env, url)
		assertStatusOK(t, "findings?db1", code)
		assertJSON(t, "findings?db1", body)
	})

	t.Run("findings_filter_db2", func(t *testing.T) {
		url := fmt.Sprintf(
			"%s/api/v1/findings?database=%s",
			env.apiBase, fleetDB2,
		)
		code, body := httpGet(t, env, url)
		assertStatusOK(t, "findings?db2", code)
		assertJSON(t, "findings?db2", body)
	})

	t.Run("findings_no_filter", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/findings",
		)
		assertStatusOK(t, "findings-all", code)
		assertJSON(t, "findings-all", body)
	})

	t.Run("config_shows_fleet", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/config",
		)
		assertStatusOK(t, "config", code)
		assertJSON(t, "config", body)
		assertContains(t, "config mode", body, "fleet")
	})

	t.Run("metrics_works", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/metrics",
		)
		assertStatusOK(t, "metrics", code)
		assertJSON(t, "metrics", body)
	})

	t.Run("emergency_stop_and_resume", func(t *testing.T) {
		code, body := httpPost(
			t, env,
			env.apiBase+"/api/v1/emergency-stop",
		)
		assertStatusOK(t, "emergency-stop", code)
		assertJSON(t, "emergency-stop", body)
		assertContains(
			t, "emergency-stop", body, "stopped",
		)

		code, body = httpPost(
			t, env, env.apiBase+"/api/v1/resume",
		)
		assertStatusOK(t, "resume", code)
		assertJSON(t, "resume", body)
		assertContains(t, "resume", body, "resumed")
	})

	t.Run("prometheus_has_pg_sage_info", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.promBase+"/metrics",
		)
		assertStatusOK(t, "prometheus", code)
		assertContains(
			t, "prometheus info", body, "pg_sage_info",
		)
	})

	t.Run("no_panics_in_stderr", func(t *testing.T) {
		stderr := env.stderr.String()
		if strings.Contains(stderr, "panic:") {
			t.Errorf(
				"binary produced panic:\n%s", stderr,
			)
		}
		if strings.Contains(stderr, "fatal error:") {
			t.Errorf(
				"binary produced fatal error:\n%s", stderr,
			)
		}
	})

	// Log output for debugging on failure.
	t.Logf(
		"stdout (truncated):\n%.2000s",
		env.stdout.String(),
	)
	t.Logf(
		"stderr (truncated):\n%.2000s",
		env.stderr.String(),
	)
}
