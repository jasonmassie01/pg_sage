package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Sinks are built from configuration; a token comes from its environment
// variable, and a missing one stops the build naming the variable only.
func TestBuildSIEMSinks(t *testing.T) {
	t.Setenv("SAGE_TEST_SIEM_TOKEN", "s3cret-token")
	sinks, err := buildSIEMSinks([]config.AuditSIEMSink{
		{Name: "soc", Type: "http", URL: "https://siem.example.com/x",
			TokenEnv: "SAGE_TEST_SIEM_TOKEN", Chains: []string{"auth_audit"}},
		{Name: "otel", Type: "otlp", URL: "http://collector:4318"},
		{Name: "rsys", Type: "syslog", Address: "127.0.0.1:6514", Network: "tls"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(sinks) != 3 || sinks[0].Sink.Name() != "soc" ||
		strings.Join(sinks[0].Chains, ",") != "auth_audit" || sinks[2].Sink.Name() != "rsys" {
		t.Fatalf("sinks = %+v", sinks)
	}
	_, err = buildSIEMSinks([]config.AuditSIEMSink{{Name: "soc", Type: "http",
		URL: "https://x", TokenEnv: "SAGE_TEST_SIEM_TOKEN_UNSET"}})
	if err == nil || !strings.Contains(err.Error(), "SAGE_TEST_SIEM_TOKEN_UNSET") {
		t.Fatalf("missing token: err = %v", err)
	}
	if _, err := buildSIEMSinks([]config.AuditSIEMSink{{Name: "x", Type: "kafka"}}); err == nil {
		t.Fatalf("unknown sink type accepted")
	}
}

// Sources: every fleet database, plus the control database unless it is
// one of them; nothing without pools.
func TestAuditSourcesDeduplicateTheControlPool(t *testing.T) {
	if got := auditSources(nil, nil)(); len(got) != 0 {
		t.Fatalf("no pools: sources = %v", got)
	}
	control := &pgxpool.Pool{}
	got := auditSources(control, nil)()
	if len(got) != 1 || got[0].Name != "control" || got[0].Pool != control {
		t.Fatalf("control only: %+v", got)
	}
}

// Verification with nothing to verify, or as a follower, does nothing.
func TestRunChainVerificationWithoutSources(t *testing.T) {
	runChainVerification(context.Background(), nil)
}
