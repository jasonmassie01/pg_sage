package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/servertls/servertlstest"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// E1 (CG-02, GR-11, readiness) through the real process: the API serves TLS
// from SAGE_TLS_CERT/SAGE_TLS_KEY, the database URL comes from
// SAGE_DATABASE_URL_FILE, and /ready reports the control database.

func tlsClient(t *testing.T, certPath string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("test certificate did not parse")
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
}

func getWith(t *testing.T, client *http.Client, url string) (int, string, *tls.ConnectionState) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.TLS
}

func TestMainChar_StandaloneServesTLSWithSecretFileAndReadiness(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "main_char_tls")
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(7001))
	dsnFile := filepath.Join(dir, "database-url")
	if err := os.WriteFile(dsnFile, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`mode: standalone
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
llm:
  enabled: false
`, api, prom))
	child := startMainChild(t, []string{
		"SAGE_DATABASE_URL_FILE=" + dsnFile,
		"SAGE_TLS_CERT=" + pair.CertPath, "SAGE_TLS_KEY=" + pair.KeyPath,
	}, "--config", path)
	child.waitForOutput(t, childStartTimeout, "[api] listening on "+api+" (TLS)")
	waitForListeners(t, 10*time.Second, api)

	client := tlsClient(t, pair.CertPath)
	code, body, state := getWith(t, client, "https://"+api+"/health")
	if code != http.StatusOK || body != `{"status":"ok"}` || state == nil ||
		state.Version < tls.VersionTLS12 ||
		state.PeerCertificates[0].SerialNumber.Int64() != 7001 {
		t.Fatalf("https /health = %d %q tls=%+v", code, body, state)
	}
	code, body, _ = getWith(t, client, "https://"+api+"/ready")
	if code != http.StatusOK || !strings.Contains(body, `"status":"ready"`) {
		t.Fatalf("https /ready = %d %q", code, body)
	}
	// Plain HTTP on the TLS port gets no API response.
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + api +
		"/health"); err == nil {
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && string(got) == `{"status":"ok"}` {
			t.Fatal("the TLS listener answered plain HTTP")
		}
	}
	if strings.Contains(child.out.String(), dsn) {
		t.Fatal("the database URL read from its file was logged")
	}
	if code := child.terminate(t); code != 0 {
		t.Fatalf("SIGTERM exit = %d", code)
	}
}

func TestMainChar_BadTLSPairExitsOneBeforeListening(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(1))
	other := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(2))
	api, prom := listenAddrs(t)
	path := writeChildConfig(t, fmt.Sprintf(`mode: standalone
postgres:
  database_url: "postgres://nobody:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
api:
  listen_addr: %q
prometheus:
  listen_addr: %q
`, api, prom))
	child := startMainChild(t, []string{
		"SAGE_TLS_CERT=" + pair.CertPath, "SAGE_TLS_KEY=" + other.KeyPath,
	}, "--config", path)
	if code := child.wait(t, 60*time.Second); code != 1 {
		t.Fatalf("bad pair exit = %d, output %q", code, child.out.String())
	}
	out := child.out.String()
	if !strings.Contains(out, "does not match") || strings.Contains(out, "listening on") {
		t.Fatalf("bad pair output = %q", out)
	}
}

func TestMainChar_SecretEnvAndFileBothSetExitsOne(t *testing.T) {
	file := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(file, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeChildConfig(t, "mode: standalone\n")
	child := startMainChild(t, []string{
		"SAGE_ENCRYPTION_KEY=from-env", "SAGE_ENCRYPTION_KEY_FILE=" + file,
	}, "--config", path)
	if code := child.wait(t, 30*time.Second); code != 1 {
		t.Fatalf("exit = %d, output %q", code, child.out.String())
	}
	out := child.out.String()
	if !strings.Contains(out, "SAGE_ENCRYPTION_KEY_FILE") || strings.Contains(out, "from-env") ||
		strings.Contains(out, "from-file") {
		t.Fatalf("output = %q", out)
	}
}
