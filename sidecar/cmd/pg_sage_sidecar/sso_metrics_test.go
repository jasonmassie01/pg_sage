package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/api"
)

// apiauth-07: /metrics shows whether SSO discovery is ready and how many
// discovery attempts failed, so an IdP outage pages before users do.

func TestWriteSSODiscoveryMetrics_Ready(t *testing.T) {
	var b strings.Builder
	writeSSODiscoveryMetrics(&b, api.SSODiscoveryStatus{Enabled: true, Ready: true,
		Failures: 3})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_sso_discovery_ready gauge\npg_sage_sso_discovery_ready 1\n",
		"# TYPE pg_sage_sso_discovery_failures_total counter\n" +
			"pg_sage_sso_discovery_failures_total 3\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
}

func TestWriteSSODiscoveryMetrics_NotReady(t *testing.T) {
	var b strings.Builder
	writeSSODiscoveryMetrics(&b, api.SSODiscoveryStatus{Enabled: true})
	out := b.String()
	if !strings.Contains(out, "pg_sage_sso_discovery_ready 0\n") ||
		!strings.Contains(out, "pg_sage_sso_discovery_failures_total 0\n") {
		t.Fatalf("not-ready metrics wrong:\n%s", out)
	}
}

func TestWriteSSODiscoveryMetrics_DisabledWritesNothing(t *testing.T) {
	var b strings.Builder
	writeSSODiscoveryMetrics(&b, api.SSODiscoveryStatus{})
	if b.Len() != 0 {
		t.Fatalf("SSO disabled still wrote metrics:\n%s", b.String())
	}
}
