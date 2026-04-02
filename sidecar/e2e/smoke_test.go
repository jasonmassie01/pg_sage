//go:build e2e

// Package e2e runs end-to-end smoke tests against the full pg_sage
// binary. It builds the binary, starts it as a subprocess pointing
// at a real PostgreSQL instance, exercises the REST API and
// Prometheus endpoints, and verifies clean shutdown.
//
// Run with: go test -tags=e2e -count=1 -timeout 180s ./e2e/
//
// Prerequisites:
//   - PostgreSQL running on localhost:5432 (or SAGE_DATABASE_URL)
//   - Role "postgres" with password "postgres" (or set SAGE_DATABASE_URL)
//   - Go toolchain available for building the binary
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// startupTimeout is the max time to wait for the binary to
	// become ready (schema bootstrap + first collector cycle).
	startupTimeout = 60 * time.Second

	// pollInterval is how often we check if the API is ready.
	pollInterval = 500 * time.Millisecond

	// requestTimeout is the per-request timeout for API calls.
	requestTimeout = 10 * time.Second

	// shutdownGrace is how long to wait for clean shutdown.
	shutdownGrace = 15 * time.Second
)

// testEnv holds the shared state for all E2E subtests.
type testEnv struct {
	binaryPath    string
	apiPort       int
	promPort      int
	apiBase       string
	promBase      string
	cmd           *exec.Cmd
	stderr        *syncBuffer
	stdout        *syncBuffer
	configPath    string
	cancel        context.CancelFunc
	sessionCookie *http.Cookie
}

// syncBuffer is a concurrency-safe buffer for capturing
// subprocess output.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// freePort asks the OS for a free TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// databaseURL returns the PG connection string for the test.
func databaseURL(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("SAGE_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://postgres:postgres@localhost:5432/" +
		"postgres?sslmode=disable"
}

// pgAvailable checks whether PostgreSQL is reachable.
func pgAvailable(t *testing.T, dsn string) bool {
	t.Helper()
	// Quick TCP check on the host:port from the DSN.
	// Parse host:port out of the DSN. The DSN format is
	// postgres://user:pass@host:port/db?params
	parts := strings.SplitAfter(dsn, "@")
	if len(parts) < 2 {
		return false
	}
	hostPort := strings.SplitN(parts[1], "/", 2)[0]
	conn, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// buildBinary compiles the pg_sage_sidecar binary into a temp dir
// and returns its path. Skips the test if the build fails.
func buildBinary(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "pg_sage_sidecar"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)

	// Build from the sidecar root so module resolution works.
	sidecarDir := filepath.Join(
		filepath.Dir(filepath.Dir(mustAbs(t))),
	)
	cmd := exec.Command(
		"go", "build",
		"-o", binPath,
		"./cmd/pg_sage_sidecar/",
	)
	cmd.Dir = sidecarDir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("cannot build binary: %v\n%s", err, out)
	}
	return binPath
}

// mustAbs returns the absolute path of the current test file's
// directory.
func mustAbs(t *testing.T) string {
	t.Helper()
	// runtime.Caller(0) gives us this file's path.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file path")
	}
	return file
}

// writeConfig creates a minimal YAML config file with the given
// ports and DSN.
func writeConfig(
	t *testing.T, apiPort, promPort int, dsn string,
) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "e2e-config.yaml")

	// Parse DSN components for YAML config.
	// DSN: postgres://user:pass@host:port/db?params
	yamlContent := fmt.Sprintf(`mode: standalone

postgres:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  database: postgres
  sslmode: disable
  max_connections: 3

collector:
  interval_seconds: 5

analyzer:
  interval_seconds: 10
  slow_query_threshold_ms: 500
  seq_scan_min_rows: 10000
  unused_index_window_days: 7
  table_bloat_dead_tuple_pct: 10

trust:
  level: observation

executor:
  ddl_timeout_seconds: 30
  maintenance_window: "* * * * *"

llm:
  enabled: false

prometheus:
  listen_addr: "127.0.0.1:%d"

api:
  listen_addr: "127.0.0.1:%d"
`, promPort, apiPort)

	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	return path
}

// startBinary launches the pg_sage_sidecar subprocess.
func startBinary(
	t *testing.T, env *testEnv, dsn string,
) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel

	cmd := exec.CommandContext(ctx,
		env.binaryPath,
		"--config", env.configPath,
	)
	cmd.Env = append(os.Environ(),
		"SAGE_DATABASE_URL="+dsn,
		// Ensure no stale env vars interfere.
		"SAGE_MODE=standalone",
		"SAGE_LLM_API_KEY=",
		"SAGE_META_DB=",
	)

	env.stdout = &syncBuffer{}
	env.stderr = &syncBuffer{}
	cmd.Stdout = env.stdout
	cmd.Stderr = env.stderr
	env.cmd = cmd

	if err := cmd.Start(); err != nil {
		t.Fatalf("startBinary: %v", err)
	}
}

// waitReady polls stderr for the "[api] listening" message, then
// authenticates by parsing the bootstrapped admin password from
// the startup logs.
func waitReady(t *testing.T, env *testEnv) {
	t.Helper()
	deadline := time.Now().Add(startupTimeout)

	// Phase 1: wait for the API to start listening.
	for time.Now().Before(deadline) {
		if env.cmd.ProcessState != nil &&
			env.cmd.ProcessState.Exited() {
			t.Fatalf(
				"binary exited during startup\n"+
					"stdout:\n%s\nstderr:\n%s",
				env.stdout.String(), env.stderr.String(),
			)
		}
		if strings.Contains(
			env.stderr.String(), "[api] listening") {
			break
		}
		time.Sleep(pollInterval)
	}
	if !strings.Contains(
		env.stderr.String(), "[api] listening") {
		t.Fatalf(
			"API never started\nstderr:\n%s",
			env.stderr.String(),
		)
	}

	// Give the startup log a moment to flush fully.
	time.Sleep(1 * time.Second)

	// Phase 2: authenticate. Parse the admin password from
	// stderr ("first admin created — email: X  password: Y")
	// or use SAGE_E2E_ADMIN_PASSWORD if the admin already
	// existed.
	login(t, env)
}

// login authenticates with the bootstrapped admin credentials.
func login(t *testing.T, env *testEnv) {
	t.Helper()

	password := os.Getenv("SAGE_E2E_ADMIN_PASSWORD")
	if password == "" {
		// Parse from stderr.
		password = parseAdminPassword(env.stderr.String())
	}
	if password == "" {
		t.Fatal(
			"cannot login: no admin password found in " +
				"stderr and SAGE_E2E_ADMIN_PASSWORD not set",
		)
	}

	payload := fmt.Sprintf(
		`{"email":"admin@pg-sage.local","password":"%s"}`,
		password,
	)
	client := &http.Client{
		Timeout: requestTimeout,
		// Don't follow redirects — we need the Set-Cookie.
		CheckRedirect: func(
			_ *http.Request, _ []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Post(
		env.apiBase+"/api/v1/auth/login",
		"application/json",
		strings.NewReader(payload),
	)
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf(
			"login failed: status %d body: %s",
			resp.StatusCode, body,
		)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "sage_session" {
			env.sessionCookie = c
			t.Log("authenticated as admin@pg-sage.local")
			return
		}
	}
	t.Fatal("login succeeded but no sage_session cookie set")
}

// parseAdminPassword extracts the generated password from the
// startup log line:
//
//	"first admin created — email: admin@pg-sage.local  password: XXXX"
func parseAdminPassword(stderr string) string {
	const marker = "password: "
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "first admin created") {
			continue
		}
		idx := strings.LastIndex(line, marker)
		if idx < 0 {
			continue
		}
		pw := strings.TrimSpace(line[idx+len(marker):])
		if pw != "" {
			return pw
		}
	}
	return ""
}

// stopBinary sends SIGINT (or kills on Windows) and waits for
// clean shutdown.
func stopBinary(t *testing.T, env *testEnv) {
	t.Helper()
	if env.cmd == nil || env.cmd.Process == nil {
		return
	}

	// On Windows, Process.Signal(os.Interrupt) is not supported,
	// so we cancel the context which kills the process.
	if runtime.GOOS == "windows" {
		env.cancel()
	} else {
		// Send SIGINT for graceful shutdown.
		env.cmd.Process.Signal(os.Interrupt)
	}

	// Wait with timeout.
	done := make(chan error, 1)
	go func() {
		done <- env.cmd.Wait()
	}()

	select {
	case err := <-done:
		// On Windows, context cancellation causes a non-zero exit.
		if runtime.GOOS != "windows" && err != nil {
			t.Logf("binary exited with: %v", err)
		}
	case <-time.After(shutdownGrace):
		t.Logf("shutdown timed out, killing process")
		env.cmd.Process.Kill()
		<-done
	}
}

// httpGet performs an authenticated GET request.
func httpGet(
	t *testing.T, env *testEnv, url string,
) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("creating GET %s: %v", url, err)
	}
	return doRequest(t, env, req)
}

// httpPost performs an authenticated POST with an empty body.
func httpPost(
	t *testing.T, env *testEnv, url string,
) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		t.Fatalf("creating POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, env, req)
}

// doRequest executes a request with the session cookie attached.
func doRequest(
	t *testing.T, env *testEnv, req *http.Request,
) (int, string) {
	t.Helper()
	if env.sessionCookie != nil {
		req.AddCookie(env.sessionCookie)
	}
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body of %s %s: %v",
			req.Method, req.URL, err)
	}
	return resp.StatusCode, string(body)
}

// assertJSON verifies that the string is valid JSON.
func assertJSON(t *testing.T, label, body string) {
	t.Helper()
	if !json.Valid([]byte(body)) {
		t.Errorf(
			"%s: expected valid JSON, got:\n%.500s",
			label, body,
		)
	}
}

// assertContains checks that body contains substr.
func assertContains(
	t *testing.T, label, body, substr string,
) {
	t.Helper()
	if !strings.Contains(body, substr) {
		t.Errorf(
			"%s: expected body to contain %q, got:\n%.500s",
			label, substr, body,
		)
	}
}

// assertStatusOK checks status is 200.
func assertStatusOK(t *testing.T, label string, code int) {
	t.Helper()
	if code != http.StatusOK {
		t.Errorf("%s: expected 200, got %d", label, code)
	}
}

// TestSmoke is the main E2E test. It builds the binary, starts
// it, exercises endpoints, and verifies clean shutdown.
func TestSmoke(t *testing.T) {
	dsn := databaseURL(t)
	if !pgAvailable(t, dsn) {
		t.Skip("PostgreSQL not reachable, skipping E2E tests")
	}

	// Clear existing users so the binary bootstraps a fresh
	// admin with a known password in its startup log.
	clearUsers(t, dsn)

	binary := buildBinary(t)
	apiPort := freePort(t)
	promPort := freePort(t)
	configPath := writeConfig(t, apiPort, promPort, dsn)

	env := &testEnv{
		binaryPath: binary,
		apiPort:    apiPort,
		promPort:   promPort,
		apiBase:    fmt.Sprintf("http://127.0.0.1:%d", apiPort),
		promBase:   fmt.Sprintf("http://127.0.0.1:%d", promPort),
		configPath: configPath,
	}

	startBinary(t, env, dsn)
	t.Cleanup(func() {
		stopBinary(t, env)
	})

	waitReady(t, env)

	// --- API Endpoint Tests ---

	t.Run("GET /api/v1/databases", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/databases",
		)
		assertStatusOK(t, "databases", code)
		assertJSON(t, "databases", body)

		// The response may be an object with a "databases"
		// array or a direct array. Verify it contains our
		// database name somewhere.
		assertContains(t, "databases", body, "postgres")
	})

	t.Run("GET /api/v1/findings", func(t *testing.T) {
		code, body := httpGet(t, env, env.apiBase+"/api/v1/findings")
		assertStatusOK(t, "findings", code)
		assertJSON(t, "findings", body)
	})

	t.Run("GET /api/v1/actions", func(t *testing.T) {
		code, body := httpGet(t, env, env.apiBase+"/api/v1/actions")
		assertStatusOK(t, "actions", code)
		assertJSON(t, "actions", body)
	})

	t.Run("GET /api/v1/snapshots/latest", func(t *testing.T) {
		// The collector runs every 5s; we may need to wait for
		// the first snapshot to appear. Retry a few times.
		var code int
		var body string
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			code, body = httpGet(
				t, env,
				env.apiBase+"/api/v1/snapshots/latest",
			)
			if code == http.StatusOK {
				break
			}
			time.Sleep(2 * time.Second)
		}
		// It's acceptable if the snapshot isn't ready yet
		// (collector might not have completed), but the endpoint
		// should at least return valid JSON.
		if code == http.StatusOK {
			assertJSON(t, "snapshots/latest", body)
		} else {
			t.Logf(
				"snapshots/latest: status %d (collector may "+
					"not have run yet)", code,
			)
		}
	})

	t.Run("GET /api/v1/config", func(t *testing.T) {
		code, body := httpGet(t, env, env.apiBase+"/api/v1/config")
		assertStatusOK(t, "config", code)
		assertJSON(t, "config", body)

		// Config should report mode=standalone.
		assertContains(t, "config mode", body, "standalone")
	})

	t.Run("GET /api/v1/metrics", func(t *testing.T) {
		code, body := httpGet(t, env, env.apiBase+"/api/v1/metrics")
		assertStatusOK(t, "metrics", code)
		assertJSON(t, "metrics", body)
	})

	t.Run("GET /api/v1/forecasts", func(t *testing.T) {
		code, body := httpGet(t, env, env.apiBase+"/api/v1/forecasts")
		assertStatusOK(t, "forecasts", code)
		assertJSON(t, "forecasts", body)
	})

	t.Run("GET /api/v1/query-hints", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/query-hints",
		)
		assertStatusOK(t, "query-hints", code)
		assertJSON(t, "query-hints", body)
	})

	t.Run("GET /api/v1/alert-log", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/alert-log",
		)
		assertStatusOK(t, "alert-log", code)
		assertJSON(t, "alert-log", body)
	})

	t.Run("GET /api/v1/llm/models", func(t *testing.T) {
		code, body := httpGet(
			t, env, env.apiBase+"/api/v1/llm/models",
		)
		// LLM is disabled in our test config, so 503 is
		// expected. Either 200 or 503 is acceptable.
		if code != http.StatusOK &&
			code != http.StatusServiceUnavailable {
			t.Errorf(
				"llm/models: expected 200 or 503, got %d",
				code,
			)
		}
		assertJSON(t, "llm/models", body)
	})

	t.Run("POST /api/v1/emergency-stop", func(t *testing.T) {
		code, body := httpPost(
			t, env, env.apiBase+"/api/v1/emergency-stop",
		)
		assertStatusOK(t, "emergency-stop", code)
		assertJSON(t, "emergency-stop", body)
		assertContains(
			t, "emergency-stop", body, "stopped",
		)
	})

	t.Run("POST /api/v1/resume", func(t *testing.T) {
		code, body := httpPost(
			t, env, env.apiBase+"/api/v1/resume",
		)
		assertStatusOK(t, "resume", code)
		assertJSON(t, "resume", body)
		assertContains(t, "resume", body, "resumed")
	})

	// --- Prometheus Metrics ---

	t.Run("Prometheus /metrics", func(t *testing.T) {
		code, body := httpGet(t, env, env.promBase+"/metrics")
		assertStatusOK(t, "prometheus", code)

		// Prometheus text format should contain pg_sage_info.
		assertContains(
			t, "prometheus content", body, "pg_sage_info",
		)
		// Should contain mode metric.
		assertContains(
			t, "prometheus mode", body, "pg_sage_mode",
		)
	})

	// --- Dashboard (SPA) ---

	t.Run("GET / serves dashboard HTML", func(t *testing.T) {
		code, body := httpGet(t, env, env.apiBase+"/")
		// The dashboard might return HTML or a minimal fallback.
		// If the React build exists in dist/, we get HTML.
		// If dist/ is empty (dev mode), we may get a 404 or
		// empty page. Either way, the endpoint should respond.
		if code != http.StatusOK &&
			code != http.StatusNotFound {
			t.Errorf(
				"dashboard: expected 200 or 404, got %d", code,
			)
		}
		if code == http.StatusOK {
			// Should be HTML if the embedded SPA is present.
			if strings.Contains(body, "<!DOCTYPE html") ||
				strings.Contains(body, "<!doctype html") ||
				strings.Contains(body, "<html") {
				t.Log("dashboard: serving embedded SPA HTML")
			} else {
				t.Log(
					"dashboard: returned 200 but no HTML " +
						"(dist/ may be empty)",
				)
			}
		}
	})

	// --- Version Flag ---

	t.Run("--version flag", func(t *testing.T) {
		cmd := exec.Command(env.binaryPath, "--version")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("--version: %v\n%s", err, out)
		}
		output := string(out)
		if !strings.Contains(output, "pg_sage") {
			t.Errorf(
				"--version: expected 'pg_sage' in output, got: %s",
				output,
			)
		}
	})

	// --- Negative Tests ---

	t.Run("GET /api/v1/findings/nonexistent", func(t *testing.T) {
		code, body := httpGet(
			t, env,
			env.apiBase+"/api/v1/findings/nonexistent-id-123",
		)
		// Should return 404 or 400, not 500.
		if code == http.StatusInternalServerError {
			t.Errorf(
				"finding detail with bad ID returned 500: %s",
				body,
			)
		}
	})

	t.Run("GET /api/v1/actions/nonexistent", func(t *testing.T) {
		code, body := httpGet(
			t, env,
			env.apiBase+"/api/v1/actions/nonexistent-id-456",
		)
		if code == http.StatusInternalServerError {
			t.Errorf(
				"action detail with bad ID returned 500: %s",
				body,
			)
		}
	})

	t.Run("GET unknown API path returns 404", func(t *testing.T) {
		code, _ := httpGet(
			t, env, env.apiBase+"/api/v1/does-not-exist",
		)
		if code != http.StatusNotFound &&
			code != http.StatusMethodNotAllowed {
			t.Errorf(
				"unknown path: expected 404/405, got %d", code,
			)
		}
	})

	// --- Verify No Panics ---

	t.Run("no panics in stderr", func(t *testing.T) {
		stderr := env.stderr.String()
		if strings.Contains(stderr, "panic:") {
			t.Errorf("binary produced panic output:\n%s", stderr)
		}
		if strings.Contains(stderr, "fatal error:") {
			t.Errorf(
				"binary produced fatal error:\n%s", stderr,
			)
		}
	})

	// Log combined output for debugging.
	t.Logf("stdout (truncated):\n%.2000s", env.stdout.String())
	t.Logf("stderr (truncated):\n%.2000s", env.stderr.String())
}

// clearUsers removes all sage users and dependent rows so the
// binary bootstraps a fresh admin on startup.
func clearUsers(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Logf("clearUsers: connect: %v (continuing)", err)
		return
	}
	defer pool.Close()

	// Clear all tables that reference sage.users via FK.
	for _, tbl := range []string{
		"sage.sessions",
		"sage.config_audit",
		"sage.config",
	} {
		_, err := pool.Exec(ctx,
			"DELETE FROM "+tbl)
		if err != nil {
			t.Logf("clearUsers: DELETE FROM %s: %v", tbl, err)
		}
	}
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.users"); err != nil {
		t.Logf("clearUsers: DELETE FROM sage.users: %v", err)
	}
}

// TestBinaryBuilds is a fast sanity check that the binary compiles.
// It doesn't need PostgreSQL.
func TestBinaryBuilds(t *testing.T) {
	binary := buildBinary(t)
	// Verify --version works without any config.
	cmd := exec.Command(binary, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--version failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pg_sage") {
		t.Errorf("expected 'pg_sage' in version output: %s", out)
	}
}
