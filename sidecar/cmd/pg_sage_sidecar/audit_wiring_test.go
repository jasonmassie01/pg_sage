package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/siem"
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

// The evidence signing key loads from its variable; unset or invalid keys
// leave packs hashed only.
func TestEvidenceSigningKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pemText, err := evidence.MarshalSigningKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	c := config.DefaultConfig()
	if evidenceSigningKey(c) != nil {
		t.Fatalf("a key without configuration")
	}
	c.Audit.Evidence.SigningKeyEnv = "SAGE_TEST_EVIDENCE_KEY"
	t.Setenv("SAGE_TEST_EVIDENCE_KEY", pemText)
	if got := evidenceSigningKey(c); got == nil || !got.Equal(priv) {
		t.Fatalf("configured key not loaded")
	}
	t.Setenv("SAGE_TEST_EVIDENCE_KEY", "not a key")
	if evidenceSigningKey(c) != nil {
		t.Fatalf("an invalid key was used")
	}
}

// SIEM status is reported only when sinks are configured, and reads the
// exporter started after the router.
func TestSIEMStatusFunc(t *testing.T) {
	c := config.DefaultConfig()
	if siemStatusFunc(c) != nil {
		t.Fatalf("status without sinks")
	}
	c.Audit.SIEM.Sinks = []config.AuditSIEMSink{{Name: "soc", Type: "http",
		URL: "https://x"}}
	f := siemStatusFunc(c)
	if f == nil {
		t.Fatalf("no status with sinks")
	}
	auditExport.Store(nil)
	if got := f(); len(got) != 0 {
		t.Fatalf("status before the exporter starts = %v", got)
	}
}

// preserveAuditFenceGlobals restores cfg and fleetLeader after a test.
func preserveAuditFenceGlobals(t *testing.T) {
	t.Helper()
	oldCfg, oldLeader, oldGov := cfg, currentFleetLeader(), governanceElector.Load()
	t.Cleanup(func() {
		cfg = oldCfg
		setFleetLeader(oldLeader)
		governanceElector.Store(oldGov)
	})
}

func yamlFleetConfig(names ...string) *config.Config {
	c := &config.Config{Mode: "fleet"}
	for _, n := range names {
		c.Databases = append(c.Databases, config.DatabaseConfig{Name: n})
	}
	return c
}

// The SIEM fence is the lease the elector holds: a reload that changes
// the YAML fleet's database list, published while export passes run,
// never moves the fence to a scope with no lease row (fleet-02).
func TestAuditLeaderFenceKeepsTheLeaseScopeAcrossPublish(t *testing.T) {
	preserveAuditFenceGlobals(t)
	cfg = yamlFleetConfig("a", "b")
	scope := fleetLearningScope(cfg)
	setFleetLeader(leader.NewElector(stubLease{held: true}, scope, "me", time.Minute))
	if err := currentFleetLeader().Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	want := siem.Fence{Scope: scope, Holder: "me", Epoch: 1}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			next := yamlFleetConfig("a", "b", "c"+strings.Repeat("x", i%7))
			fleetDatabaseSettings{active: next}.publish()
		}
	}()
	for i := 0; i < 300; i++ {
		if got, ok := auditLeaderFence(); !ok || got != want {
			t.Errorf("pass %d: fence = %+v,%v, want %+v", i, got, ok, want)
			break
		}
	}
	wg.Wait()
	if got, ok := auditLeaderFence(); !ok || got != want {
		t.Fatalf("after the reload: fence = %+v,%v, want %+v (lease scope)", got, ok, want)
	}
	if fleetLearningScope(cfg) == scope {
		t.Fatal("fixture: the published fleet must hash to another scope")
	}
}

// Without election export runs unfenced; a follower does not export.
func TestAuditLeaderFenceDisabledAndFollower(t *testing.T) {
	preserveAuditFenceGlobals(t)
	cfg = yamlFleetConfig("a")
	setFleetLeader(nil)
	if f, ok := auditLeaderFence(); !ok || f != (siem.Fence{}) {
		t.Fatalf("election disabled: fence = %+v,%v, want unfenced pass", f, ok)
	}
	setFleetLeader(leader.NewElector(stubLease{held: false}, "s", "me", time.Minute))
	_ = currentFleetLeader().Tick(context.Background())
	if f, ok := auditLeaderFence(); ok || f != (siem.Fence{}) {
		t.Fatalf("follower: fence = %+v,%v, want no pass", f, ok)
	}
	cfg = nil
	setFleetLeader(leader.NewElector(stubLease{held: true}, "s", "me", time.Minute))
	_ = currentFleetLeader().Tick(context.Background())
	want := siem.Fence{Scope: "s", Holder: "me", Epoch: 1}
	if f, ok := auditLeaderFence(); !ok || f != want {
		t.Fatalf("nil config: fence = %+v,%v, want %+v", f, ok, want)
	}
}

// A stub from a source that left the fleet cannot be recorded: the report
// fails naming the source, so the exporter holds that lane's cursor.
func TestSIEMUndeliverableUnknownSource(t *testing.T) {
	report := siemUndeliverable(func() []auditjob.Source {
		return []auditjob.Source{{Name: "orders"}}
	})
	err := report(context.Background(), siem.Undeliverable{Sink: "soc", Source: "billing",
		Chain: "action_log", Seq: 4})
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("unknown source: err = %v, want an error naming billing", err)
	}
	err = report(context.Background(), siem.Undeliverable{Sink: "soc", Source: "orders",
		Chain: "action_log", Seq: 4})
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("source without a pool: err = %v, want an error naming orders", err)
	}
}
