package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/api"
)

// apiauth-06: an X-Forwarded-For from a peer outside api.trusted_proxies
// is still ignored, but the operator hears about it once.

func resetUntrustedXFFWarning(t *testing.T) {
	t.Helper()
	untrustedXFFWarning = sync.Once{}
	t.Cleanup(func() { untrustedXFFWarning = sync.Once{} })
}

// xffServer serves the rate-limited stack over real TCP from loopback.
func xffServer(t *testing.T) *httptest.Server {
	t.Helper()
	rl := NewRateLimiter(100)
	t.Cleanup(rl.Stop)
	srv := httptest.NewServer(rateLimitMiddleware(rl, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))
	t.Cleanup(srv.Close)
	return srv
}

func getWithXFF(t *testing.T, srv *httptest.Server, xff string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestClientIPWarnsOnceForUntrustedForwardedFor(t *testing.T) {
	restore := snapshotTrustedProxyNets()
	t.Cleanup(restore)
	setTrustedProxies([]string{"192.0.2.1"}) // loopback is now untrusted
	resetUntrustedXFFWarning(t)
	srv := xffServer(t)
	logs := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			getWithXFF(t, srv, "203.0.113.9")
		}
	})
	if got := strings.Count(logs, "api.trusted_proxies"); got != 1 {
		t.Fatalf("warnings = %d, want exactly 1:\n%s", got, logs)
	}
	if !strings.Contains(logs, "[WARN]") || !strings.Contains(logs, "127.0.0.1") {
		t.Fatalf("warning lacks the WARN level or the peer:\n%s", logs)
	}
	if strings.Contains(logs, "203.0.113.9") {
		t.Fatalf("warning echoed the client-controlled header:\n%s", logs)
	}
}

func TestClientIPNoWarningWithoutHeaderOrFromTrustedPeer(t *testing.T) {
	restore := snapshotTrustedProxyNets()
	t.Cleanup(restore)
	resetUntrustedXFFWarning(t)
	srv := xffServer(t)
	setTrustedProxies([]string{"192.0.2.1"})
	quiet := captureStderr(t, func() { getWithXFF(t, srv, "") })
	setTrustedProxies(nil) // default: loopback is trusted
	trusted := captureStderr(t, func() { getWithXFF(t, srv, "203.0.113.9") })
	if strings.Contains(quiet+trusted, "api.trusted_proxies") {
		t.Fatalf("unexpected warning: no header=%q trusted peer=%q", quiet, trusted)
	}
}

func TestMetricsPublishLoginLimiterSaturation(t *testing.T) {
	preserveValueGlobals(t)
	installValueFleet(nil)
	body := scrapeMetrics(t)
	want := "# TYPE pg_sage_login_limiter_saturated_total counter\n" +
		"pg_sage_login_limiter_saturated_total "
	if !strings.Contains(body, want) {
		t.Fatalf("metrics lack the login limiter saturation counter:\n%s", body)
	}
	var b strings.Builder
	writeLoginLimiterMetric(&b, 7)
	if !strings.Contains(b.String(), "pg_sage_login_limiter_saturated_total 7\n") {
		t.Fatalf("writer output = %q, want the value 7", b.String())
	}
	if got := api.LoginLimiterSaturatedTotal(); !strings.Contains(body,
		"pg_sage_login_limiter_saturated_total "+strconv.FormatUint(got, 10)) {
		t.Fatalf("scrape does not publish the live value %d", got)
	}
}
