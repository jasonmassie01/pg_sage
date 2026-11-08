package agentguard

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestReplicaConnectionValidatesNativeEndpointBeforeConnecting(t *testing.T) {
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"db.example":"projects/invalid"}`)
	s := &Switch{cfg: DefaultKillConfig()}
	run := s.openReplica(t.Context(), "primary", Replica{Name: "replica",
		DSN: "postgres://test@db.example/test?sslmode=require"})
	if run.conn != nil || !strings.Contains(run.report.Error, "invalid Lakebase endpoint resource path") {
		t.Fatalf("replica bypassed native endpoint validation: %+v", run.report)
	}
}

// This verifies only the replica connection/bootstrap path, without invoking
// session termination or role changes. A real standby kill needs its own test.
func TestLakebaseReplicaConnectionLive(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE=1 and SAGE_LAKEBASE_LIVE_DATABASE_URL")
	}
	dsn := os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL")
	if dsn == "" {
		t.Fatal("live replica connection test requires a passwordless Lakebase DSN")
	}
	cfg := DefaultKillConfig()
	cfg.ReplicaConnectTimeout = 30 * time.Second
	s := &Switch{cfg: cfg}
	run := s.openReplica(t.Context(), "primary", Replica{Name: "test-connection", DSN: dsn})
	if run.conn == nil || run.report.Error != "" {
		t.Fatalf("native replica connection failed: %s", run.report.Error)
	}
	defer run.close()
	var tls bool
	if err := run.conn.QueryRow(t.Context(), "SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()").Scan(&tls); err != nil || !tls {
		t.Fatalf("native replica connection did not verify TLS: %v", err)
	}
	t.Log("native OAuth replica connection/bootstrap passed; no kill actions performed")
}
