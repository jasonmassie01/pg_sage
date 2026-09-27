//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// G10-I01: run the binary exactly as README.md and docs/installation.md
// show, with no config file, no mode and no other settings, and prove the
// quick start monitors the database. The --pg-url form previously started
// only the API (G10-B01).
const (
	documentedAPIAddr  = "127.0.0.1:8080" // DefaultAPIListenAddr
	documentedPromAddr = "127.0.0.1:9187" // DefaultPrometheusListenAddr
	// The first collector cycle runs one default interval (60s) after start.
	firstSnapshotTimeout = 100 * time.Second
)

func TestDocumentedQuickStart(t *testing.T) {
	admin := adminDSN(t)
	if !pgAvailable(t, admin) {
		t.Fatal("PostgreSQL not reachable; the documented-path smoke needs " +
			"SAGE_TEST_DATABASE_URL")
	}
	requireDefaultPortsFree(t)
	binary := buildBinary(t)

	t.Run("pg-url flag only", func(t *testing.T) {
		dsn := freshDatabase(t, admin)
		env := startDocumented(t, binary, []string{"--pg-url", dsn}, nil)
		assertMonitoringStandalone(t, env)
		assertFirstSnapshot(t, dsn)
	})
	t.Run("SAGE_DATABASE_URL only (docker run form)", func(t *testing.T) {
		dsn := freshDatabase(t, admin)
		env := startDocumented(t, binary, nil, []string{"SAGE_DATABASE_URL=" + dsn})
		assertMonitoringStandalone(t, env)
	})
}

// requireDefaultPortsFree fails early with a clear message: the documented
// path cannot choose its ports.
func requireDefaultPortsFree(t *testing.T) {
	t.Helper()
	for _, addr := range []string{documentedAPIAddr, documentedPromAddr} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("documented default port %s is in use: %v", addr, err)
		}
		_ = l.Close()
	}
}

// freshDatabase creates an empty database prepared by running the "Database
// User Setup" SQL from docs/installation.md verbatim, and returns a DSN for
// that least-privileged role.
func freshDatabase(t *testing.T, admin string) string {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(pool.Close)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	name, role := "sage_docpath_"+suffix, "sage_agent_"+suffix
	password := randomHex(t)
	t.Cleanup(func() {
		mustExecAdmin(t, pool, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	})
	createTestDB(t, pool, name)
	t.Cleanup(func() { dropTestDB(t, pool, name) })
	dbPool, err := pgxpool.New(context.Background(), withDatabase(t, admin, name, nil))
	if err != nil {
		t.Fatalf("fixture pool: %v", err)
	}
	defer dbPool.Close()
	for _, stmt := range documentedSetupSQL(t, role, password) {
		mustExecAdmin(t, dbPool, stmt)
	}
	return withDatabase(t, admin, name, url.UserPassword(role, password))
}

// documentedSetupSQL extracts the first sql block under "## Database User
// Setup" in docs/installation.md, substituting the role and password.
// CREATE USER is cluster-wide; the rest applies to the monitored database.
func documentedSetupSQL(t *testing.T, role, password string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/installation.md")
	if err != nil {
		t.Fatalf("read installation docs: %v", err)
	}
	_, section, found := strings.Cut(string(raw), "## Database User Setup")
	_, block, fenced := strings.Cut(section, "```sql")
	block, _, closed := strings.Cut(block, "```")
	if !found || !fenced || !closed {
		t.Fatal("docs/installation.md lost its Database User Setup sql block")
	}
	block = strings.NewReplacer("sage_agent", role, "YOUR_PASSWORD", password).Replace(block)
	var statements []string
	for _, stmt := range strings.Split(stripSQLComments(block), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			statements = append(statements, stmt)
		}
	}
	if len(statements) < 5 {
		t.Fatalf("documented setup parsed into %d statements: %q", len(statements), statements)
	}
	return statements
}

func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

func withDatabase(t *testing.T, dsn, database string, user *url.Userinfo) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + database
	if user != nil {
		u.User = user
	}
	return u.String()
}

func mustExecAdmin(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("fixture SQL failed: %v", err)
	}
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// startDocumented runs the binary from an empty directory (no config.yaml
// to auto-detect) with every SAGE_* variable removed except extra.
func startDocumented(t *testing.T, binary string, args, extra []string) *testEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(withoutSageVars(os.Environ()), extra...)
	env := &testEnv{
		binaryPath: binary, cmd: cmd, cancel: cancel,
		apiBase:  "http://" + documentedAPIAddr,
		promBase: "http://" + documentedPromAddr,
		stdout:   &syncBuffer{}, stderr: &syncBuffer{},
	}
	cmd.Stdout, cmd.Stderr = env.stdout, env.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start documented binary: %v", err)
	}
	t.Cleanup(func() { stopBinary(t, env) })
	waitReady(t, env)
	return env
}

func withoutSageVars(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "SAGE_") {
			kept = append(kept, kv)
		}
	}
	return kept
}

func assertMonitoringStandalone(t *testing.T, env *testEnv) {
	t.Helper()
	code, body := httpGet(t, env, env.apiBase+"/api/v1/config")
	assertStatusOK(t, "config", code)
	assertContains(t, "config mode", body, "standalone")
	code, body = httpGet(t, env, env.apiBase+"/api/v1/databases")
	assertStatusOK(t, "databases", code)
	if !strings.Contains(body, "sage_docpath_") {
		t.Fatalf("the monitored database is not registered: %s", body)
	}
}

// assertFirstSnapshot proves the collector runs, not just the API.
func assertFirstSnapshot(t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("monitored pool: %v", err)
	}
	defer pool.Close()
	deadline := time.Now().Add(firstSnapshotTimeout)
	for time.Now().Before(deadline) {
		var n int
		err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM sage.snapshots").Scan(&n)
		if err == nil && n > 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no collector snapshot within %s of a documented start", firstSnapshotTimeout)
}
