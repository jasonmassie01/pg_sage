package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Mode-by-mode characterization of main(): which components start for a
// given config, what the API and Prometheus listeners serve, and the exit
// code of each stop path. See main_process_char_test.go for the harness.

const childStartTimeout = 90 * time.Second

func TestMainChar_VersionFlagPrintsBuildInfoAndExitsZero(t *testing.T) {
	child := startMainChild(t, nil, "--version")
	if code := child.wait(t, 30*time.Second); code != 0 {
		t.Fatalf("--version exit = %d, output %q", code, child.out.String())
	}
	out := child.out.String()
	if !strings.Contains(out, "pg_sage dev (commit: none, built: unknown, sql-ast: ") {
		t.Fatalf("--version output = %q", out)
	}
	if strings.Contains(out, "[startup]") {
		t.Fatalf("--version started the sidecar: %q", out)
	}
}

func TestMainChar_InvalidModeExitsOneBeforeConnecting(t *testing.T) {
	path := writeChildConfig(t, "mode: bogus\n")
	child := startMainChild(t, nil, "--config", path)
	if code := child.wait(t, 30*time.Second); code != 1 {
		t.Fatalf("invalid mode exit = %d, output %q", code, child.out.String())
	}
	out := child.out.String()
	if !strings.Contains(out, "[ERROR] [startup] config: ") || strings.Contains(out, "connected") {
		t.Fatalf("invalid mode output = %q", out)
	}
}

func TestMainChar_UnreachableStandaloneDatabaseExitsOne(t *testing.T) {
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`mode: standalone
postgres:
  database_url: "postgres://nobody:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
`, api, prom))
	child := startMainChild(t, nil, "--config", path)
	if code := child.wait(t, 60*time.Second); code != 1 {
		t.Fatalf("unreachable database exit = %d, output %q", code, child.out.String())
	}
	out := child.out.String()
	if !strings.Contains(out, "[ERROR] [startup]") || strings.Contains(out, "listening on") {
		t.Fatalf("unreachable database output = %q", out)
	}
}

func TestMainChar_StandaloneStartsEveryComponentAndRestartsUnderSupervisor(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "main_char_standalone")
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`mode: standalone
postgres:
  database_url: %q
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
llm:
  enabled: false
`, dsn, api, prom))
	child := startMainChild(t, []string{"SAGE_SUPERVISED=1"}, "--config", path)
	child.waitForOutput(t, childStartTimeout, "[api] listening on "+api,
		"[prometheus] listening on "+prom)
	waitForListeners(t, 10*time.Second, api, prom)
	out := child.out.String()
	for _, want := range []string{
		"[startup] connected to PostgreSQL", "[startup] cloud environment: ",
		"[startup] first admin created", "[startup] standalone mode initialized",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q", want)
		}
	}
	if strings.Contains(out, "restart endpoint disabled") {
		t.Error("supervised child disabled the restart endpoint")
	}
	assertServing(t, api, prom, "standalone")
	cookie := adminSession(t, api, adminPasswordFrom(t, out))
	if code := postRestart(t, api, cookie); code != http.StatusOK {
		t.Fatalf("restart = %d, want 200", code)
	}
	if code := child.wait(t, 30*time.Second); code != restartExitCode {
		t.Fatalf("restart exit = %d, want %d; output:\n%s", code, restartExitCode,
			redactAdminPassword(child.out.String()))
	}
	assertOrderedLog(t, child.out.String(), "shutting down", "[shutdown] stopped",
		fmt.Sprintf("restarting (exit %d)", restartExitCode))
}

func TestMainChar_FleetStartsPerDatabaseRuntimeAndStopsOnSIGTERM(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "main_char_fleet")
	conn, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`mode: fleet
databases:
  - name: alpha
    host: %q
    port: %d
    user: %q
    password: %q
    database: %q
    sslmode: disable
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
llm:
  enabled: false
`, conn.Host, conn.Port, conn.User, conn.Password, conn.Database, api, prom))
	child := startMainChild(t, nil, "--config", path)
	child.waitForOutput(t, childStartTimeout, "[api] listening on "+api,
		"[prometheus] listening on "+prom)
	waitForListeners(t, 10*time.Second, api, prom)
	out := child.out.String()
	for _, want := range []string{
		"[fleet] 1 of 1 configured databases initialized", "restart endpoint disabled",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startup log lacks %q", want)
		}
	}
	if strings.Contains(out, "connected to PostgreSQL") {
		t.Error("fleet mode opened the standalone monitored pool")
	}
	assertServing(t, api, prom, "fleet")
	if _, body := httpGet(t, "http://"+prom+"/metrics"); !strings.Contains(body,
		"pg_sage_fleet_databases 1") {
		t.Errorf("fleet metrics lack the database count:\n%s", body)
	}
	if code := child.terminate(t); code != 0 {
		t.Fatalf("SIGTERM exit = %d", code)
	}
	assertOrderedLog(t, child.out.String(), "received terminated, shutting down",
		"[shutdown] stopped")
}

func TestMainChar_MetaModeInitializesMetaDatabase(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "main_char_meta")
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`meta_db: %q
encryption_key: "characterization-test-passphrase-0123456789"
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
llm:
  enabled: false
`, dsn, api, prom))
	child := startMainChild(t, nil, "--config", path)
	child.waitForOutput(t, childStartTimeout, "[api] listening on "+api)
	waitForListeners(t, 10*time.Second, api)
	out := child.out.String()
	if !strings.Contains(out, "[startup] meta database initialized") ||
		strings.Contains(out, "connected to PostgreSQL") {
		t.Fatalf("meta startup log:\n%s", redactAdminPassword(out))
	}
	if code, body := httpGet(t, "http://"+api+"/health"); code != http.StatusOK ||
		body != `{"status":"ok"}` {
		t.Fatalf("/health = %d %q", code, body)
	}
	if code := child.terminate(t); code != 0 {
		t.Fatalf("SIGTERM exit = %d", code)
	}
}

func assertServing(t *testing.T, api, prom, mode string) {
	t.Helper()
	if code, body := httpGet(t, "http://"+api+"/health"); code != http.StatusOK ||
		body != `{"status":"ok"}` {
		t.Errorf("/health = %d %q", code, body)
	}
	if code, _ := httpGet(t, "http://"+api+"/api/v1/databases"); code !=
		http.StatusUnauthorized {
		t.Errorf("unauthenticated API = %d, want 401", code)
	}
	code, body := httpGet(t, "http://"+prom+"/metrics")
	want := fmt.Sprintf("pg_sage_info{version=%q,mode=%q} 1", "dev", mode)
	if code != http.StatusOK || !strings.Contains(body, want) ||
		!strings.Contains(body, "pg_sage_connection_up 1") {
		t.Errorf("/metrics = %d, want %q and connection_up 1:\n%s", code, want, body)
	}
}

func adminSession(t *testing.T, api, password string) []*http.Cookie {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":%q}`, adminEmail, password)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(
		"http://"+api+"/api/v1/auth/login", "application/json",
		bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.Cookies()) == 0 {
		t.Fatalf("login = %d with %d cookies", resp.StatusCode, len(resp.Cookies()))
	}
	return resp.Cookies()
}

func postRestart(t *testing.T, api string, cookies []*http.Cookie) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+api+"/api/v1/restart",
		bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func assertOrderedLog(t *testing.T, out string, want ...string) {
	t.Helper()
	at := 0
	for _, w := range want {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("log lacks %q after offset %d:\n%s", w, at, redactAdminPassword(out))
		}
		at += i + len(w)
	}
}
