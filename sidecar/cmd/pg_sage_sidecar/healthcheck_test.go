package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No concurrent access tests: the healthcheck is a one-shot process with no
// shared state.

// isolateHealthcheck clears every input the healthcheck reads and moves into
// an empty directory, so no ./config.yaml or ambient TLS pair leaks in.
func isolateHealthcheck(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, name := range []string{"SAGE_CONFIG_PATH", "SAGE_TLS_CERT", "SAGE_TLS_KEY"} {
		t.Setenv(name, "")
	}
}

func runHealthcheckForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runHealthcheck(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func healthServer(t *testing.T, tlsOn bool, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	if tlsOn {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

func hostPort(srv *httptest.Server) string {
	return srv.Listener.Addr().String()
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func writeHealthcheckYAML(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestHealthcheckPlainHTTPHealthy(t *testing.T) {
	isolateHealthcheck(t)
	t.Setenv("SAGE_API_KEY", "must-not-be-sent")
	var gotPath, gotAuth, gotCookie string
	srv := healthServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotCookie = r.URL.Path, r.Header.Get("Authorization"),
			r.Header.Get("Cookie")
		okHandler(w, r)
	})
	code, stdout, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if gotPath != "/health" {
		t.Errorf("probed path %q, want /health", gotPath)
	}
	if gotAuth != "" || gotCookie != "" {
		t.Errorf("healthcheck sent credentials: Authorization=%q Cookie=%q", gotAuth, gotCookie)
	}
	want := "http://" + hostPort(srv) + "/health"
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout %q does not name %s", stdout, want)
	}
}

func TestHealthcheckUsesHTTPSWhenTLSPairSet(t *testing.T) {
	isolateHealthcheck(t)
	t.Setenv("SAGE_TLS_CERT", "/etc/pg_sage/tls.crt")
	t.Setenv("SAGE_TLS_KEY", "/etc/pg_sage/tls.key")
	srv := healthServer(t, true, okHandler)
	code, stdout, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
	if code != 0 {
		t.Fatalf("exit = %d, want 0 against a TLS listener; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "https://"+hostPort(srv)+"/health") {
		t.Errorf("stdout %q does not name the https URL", stdout)
	}
}

// TestHealthcheckPlainProbeFailsAgainstTLSListener is the defect the curl
// probe had: without the TLS pair the check speaks HTTP and a TLS-only
// listener refuses it.
func TestHealthcheckPlainProbeFailsAgainstTLSListener(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, true, okHandler)
	code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for HTTP against TLS", code)
	}
	if !strings.Contains(stderr, "http://"+hostPort(srv)+"/health") {
		t.Errorf("stderr %q does not name the URL", stderr)
	}
}

func TestHealthcheckTLSProbeFailsAgainstPlainListener(t *testing.T) {
	isolateHealthcheck(t)
	t.Setenv("SAGE_TLS_CERT", "c.crt")
	t.Setenv("SAGE_TLS_KEY", "c.key")
	srv := healthServer(t, false, okHandler)
	code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for HTTPS against plain HTTP", code)
	}
	if !strings.Contains(stderr, "https://"+hostPort(srv)+"/health") {
		t.Errorf("stderr %q does not name the URL", stderr)
	}
}

func TestHealthcheckRejectsHalfTLSPair(t *testing.T) {
	for _, set := range []string{"SAGE_TLS_CERT", "SAGE_TLS_KEY"} {
		t.Run(set, func(t *testing.T) {
			isolateHealthcheck(t)
			t.Setenv(set, "only-one.pem")
			srv := healthServer(t, false, okHandler)
			code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
			if code != 1 {
				t.Fatalf("exit = %d, want 1 with only %s set", code, set)
			}
			if !strings.Contains(stderr, "SAGE_TLS_CERT and SAGE_TLS_KEY") {
				t.Errorf("stderr %q does not name the TLS pair", stderr)
			}
		})
	}
}

func TestHealthcheckInvalidAddrFlag(t *testing.T) {
	cases := []string{"nohost", "8080", "127.0.0.1:", "127.0.0.1:abc",
		"127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:-1"}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			isolateHealthcheck(t)
			code, _, stderr := runHealthcheckForTest(t, "--addr", addr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for --addr %q", code, addr)
			}
			if !strings.Contains(stderr, "api.listen_addr") ||
				!strings.Contains(stderr, addr) {
				t.Errorf("stderr %q does not name api.listen_addr and %q", stderr, addr)
			}
		})
	}
}

func TestHealthcheckInvalidYAMLListenAddr(t *testing.T) {
	isolateHealthcheck(t)
	path := writeHealthcheckYAML(t, t.TempDir(), "api:\n  listen_addr: \"8080\"\n")
	t.Setenv("SAGE_CONFIG_PATH", path)
	code, _, stderr := runHealthcheckForTest(t)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for listen_addr without a colon", code)
	}
	for _, want := range []string{"api.listen_addr", path, "8080"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not name %q", stderr, want)
		}
	}
}

func TestHealthcheckUnreadableConfigPath(t *testing.T) {
	isolateHealthcheck(t)
	path := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv("SAGE_CONFIG_PATH", path)
	code, _, stderr := runHealthcheckForTest(t)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for an unreadable SAGE_CONFIG_PATH", code)
	}
	if !strings.Contains(stderr, path) {
		t.Errorf("stderr %q does not name %s", stderr, path)
	}
}

func TestHealthcheckMalformedConfigYAML(t *testing.T) {
	isolateHealthcheck(t)
	path := writeHealthcheckYAML(t, t.TempDir(), "api: [unterminated\n")
	t.Setenv("SAGE_CONFIG_PATH", path)
	code, _, stderr := runHealthcheckForTest(t)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for malformed YAML", code)
	}
	if !strings.Contains(stderr, path) {
		t.Errorf("stderr %q does not name %s", stderr, path)
	}
}

func TestHealthcheckInvalidFlags(t *testing.T) {
	cases := map[string][]string{
		"zero timeout":     {"--timeout", "0s"},
		"negative timeout": {"--timeout", "-1s"},
		"garbage timeout":  {"--timeout", "soon"},
		"unknown flag":     {"--port", "8080"},
		"positional arg":   {"extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			isolateHealthcheck(t)
			code, _, stderr := runHealthcheckForTest(t, args...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 for %v", code, args)
			}
			if !strings.Contains(stderr, "healthcheck") {
				t.Errorf("stderr %q does not say which command failed", stderr)
			}
		})
	}
}

func TestHealthcheckServerErrorStatus(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a 500", code)
	}
	for _, want := range []string{"status 500", "http://" + hostPort(srv) + "/health"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not contain %q", stderr, want)
		}
	}
}

func TestHealthcheckDoesNotFollowRedirects(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		okHandler(w, r)
	})
	code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a redirect", code)
	}
	if !strings.Contains(stderr, "status 302") {
		t.Errorf("stderr %q does not name status 302", stderr)
	}
}

func TestHealthcheckConnectionRefused(t *testing.T) {
	isolateHealthcheck(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	code, _, stderr := runHealthcheckForTest(t, "--addr", addr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a refused connection", code)
	}
	if !strings.Contains(stderr, "http://"+addr+"/health") ||
		!strings.Contains(stderr, "refused") {
		t.Errorf("stderr %q does not name the URL and the refusal", stderr)
	}
	if strings.Contains(stderr, "status") {
		t.Errorf("stderr %q reports a status for a refused connection", stderr)
	}
}

func TestHealthcheckTimeoutBoundary(t *testing.T) {
	isolateHealthcheck(t)
	release := make(chan struct{})
	srv := healthServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		okHandler(w, r)
	})
	defer close(release)
	start := time.Now()
	code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv),
		"--timeout", "200ms")
	elapsed := time.Since(start)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a handler slower than the timeout", code)
	}
	if elapsed < 150*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Errorf("healthcheck took %s, want about 200ms", elapsed)
	}
	if !strings.Contains(stderr, "within 200ms") {
		t.Errorf("stderr %q does not name the 200ms timeout", stderr)
	}
}

func TestHealthcheckAnswerJustInsideTimeout(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		okHandler(w, r)
	})
	code, _, stderr := runHealthcheckForTest(t, "--addr", hostPort(srv), "--timeout", "2s")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 for an answer inside the timeout; stderr: %s",
			code, stderr)
	}
}

func TestHealthcheckWildcardDialsLoopback(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, false, okHandler)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	for _, addr := range []string{
		fmt.Sprintf("0.0.0.0:%d", port), fmt.Sprintf(":%d", port),
		fmt.Sprintf("[::]:%d", port),
	} {
		code, stdout, stderr := runHealthcheckForTest(t, "--addr", addr)
		if code != 0 {
			t.Fatalf("--addr %s: exit = %d, want 0; stderr: %s", addr, code, stderr)
		}
		want := fmt.Sprintf("http://127.0.0.1:%d/health", port)
		if !strings.Contains(stdout, want) {
			t.Errorf("--addr %s: stdout %q does not name %s", addr, stdout, want)
		}
	}
}

func TestHealthcheckURLMapping(t *testing.T) {
	cases := []struct {
		addr  string
		https bool
		want  string
	}{
		{"0.0.0.0:9090", false, "http://127.0.0.1:9090/health"},
		{":9090", false, "http://127.0.0.1:9090/health"},
		{"[::]:9090", true, "https://127.0.0.1:9090/health"},
		{"localhost:8080", false, "http://localhost:8080/health"},
		{"[::1]:8443", true, "https://[::1]:8443/health"},
		{"10.0.0.5:8080", true, "https://10.0.0.5:8080/health"},
		{"127.0.0.1:1", false, "http://127.0.0.1:1/health"},
		{"127.0.0.1:65535", false, "http://127.0.0.1:65535/health"},
	}
	for _, tc := range cases {
		target, err := healthcheckTargetFor(tc.addr, tc.https)
		if err != nil {
			t.Errorf("healthcheckTargetFor(%q): %v", tc.addr, err)
			continue
		}
		if target.url != tc.want {
			t.Errorf("healthcheckTargetFor(%q) = %q, want %q", tc.addr, target.url, tc.want)
		}
	}
}

func TestHealthcheckDefaultTargetWithNoConfig(t *testing.T) {
	isolateHealthcheck(t)
	target, err := resolveHealthcheckTarget("")
	if err != nil {
		t.Fatalf("resolveHealthcheckTarget: %v", err)
	}
	if target.url != "http://127.0.0.1:8080/health" {
		t.Fatalf("default target = %q, want http://127.0.0.1:8080/health", target.url)
	}
}

func TestHealthcheckTargetPrecedence(t *testing.T) {
	isolateHealthcheck(t)
	writeHealthcheckYAML(t, ".", "api:\n  listen_addr: 0.0.0.0:7001\n")
	target, err := resolveHealthcheckTarget("")
	if err != nil || target.url != "http://127.0.0.1:7001/health" {
		t.Fatalf("./config.yaml target = %q, %v; want port 7001", target.url, err)
	}
	envPath := writeHealthcheckYAML(t, t.TempDir(), "api:\n  listen_addr: \":7002\"\n")
	t.Setenv("SAGE_CONFIG_PATH", envPath)
	target, err = resolveHealthcheckTarget("")
	if err != nil || target.url != "http://127.0.0.1:7002/health" {
		t.Fatalf("SAGE_CONFIG_PATH target = %q, %v; want port 7002", target.url, err)
	}
	target, err = resolveHealthcheckTarget("127.0.0.1:7003")
	if err != nil || target.url != "http://127.0.0.1:7003/health" {
		t.Fatalf("--addr target = %q, %v; want port 7003", target.url, err)
	}
}

func TestHealthcheckYAMLWithoutListenAddrUsesDefault(t *testing.T) {
	isolateHealthcheck(t)
	path := writeHealthcheckYAML(t, t.TempDir(), "mode: standalone\napi:\n  listen_addr: \"\"\n")
	t.Setenv("SAGE_CONFIG_PATH", path)
	target, err := resolveHealthcheckTarget("")
	if err != nil {
		t.Fatalf("resolveHealthcheckTarget: %v", err)
	}
	if target.url != "http://127.0.0.1:8080/health" {
		t.Fatalf("target = %q, want the default port 8080", target.url)
	}
}

func TestHealthcheckSkipsVerifyOnlyForLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8443": true, "localhost:8443": true, "[::1]:8443": true,
		"0.0.0.0:8443": true, "10.0.0.5:8443": false, "sage.example.com:8443": false,
	}
	for addr, wantSkip := range cases {
		target, err := healthcheckTargetFor(addr, true)
		if err != nil {
			t.Fatalf("healthcheckTargetFor(%q): %v", addr, err)
		}
		client := healthcheckClient(target, time.Second)
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport is %T, want *http.Transport", addr, client.Transport)
		}
		if transport.Proxy != nil {
			t.Errorf("%s: healthcheck honours proxy settings", addr)
		}
		cfg := transport.TLSClientConfig
		if cfg == nil {
			t.Fatalf("%s: no TLS client config", addr)
		}
		if cfg.InsecureSkipVerify != wantSkip {
			t.Errorf("%s: InsecureSkipVerify = %t, want %t", addr, cfg.InsecureSkipVerify,
				wantSkip)
		}
		if cfg.MinVersion < tls.VersionTLS12 {
			t.Errorf("%s: MinVersion = %x, want at least TLS 1.2", addr, cfg.MinVersion)
		}
	}
}

func TestHealthcheckRejectsUntrustedNonLoopbackCertificate(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, true, okHandler)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	// A non-loopback name for the same listener: the self-signed test
	// certificate must not be accepted there.
	target, err := healthcheckTargetFor(fmt.Sprintf("sage.invalid:%d", port), true)
	if err != nil {
		t.Fatalf("healthcheckTargetFor: %v", err)
	}
	client := healthcheckClient(target, time.Second)
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	err = probeHealth(client, target, time.Second)
	if err == nil {
		t.Fatal("probe accepted an untrusted certificate on a non-loopback target")
	}
	// The failure must be the certificate check, not a dial or timeout.
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("probe error %q does not come from certificate verification", err)
	}
}

func TestHealthcheckSubcommandDispatch(t *testing.T) {
	isolateHealthcheck(t)
	srv := healthServer(t, false, okHandler)
	child := startMainChild(t, nil, "healthcheck", "--addr", hostPort(srv))
	if code := child.wait(t, 30*time.Second); code != 0 {
		t.Fatalf("sage-sidecar healthcheck exit = %d, want 0; output:\n%s", code,
			child.out.String())
	}
	closed := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	child = startMainChild(t, nil, "healthcheck", "--addr", closed)
	if code := child.wait(t, 30*time.Second); code != 1 {
		t.Fatalf("sage-sidecar healthcheck on a closed port exit = %d, want 1", code)
	}
	if !strings.Contains(child.out.String(), closed) {
		t.Errorf("output %q does not name %s", child.out.String(), closed)
	}
}
