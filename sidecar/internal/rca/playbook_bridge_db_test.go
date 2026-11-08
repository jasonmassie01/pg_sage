//go:build integration

package rca

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// replicationLagSnapshot fires only replication_lag_increasing: one replica
// 90s behind (threshold 30s), connections and cache well under threshold.
func replicationLagSnapshot() *collector.Snapshot {
	lag, addr := "00:01:30", "10.0.0.9"
	return &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{TotalBackends: 5, MaxConnections: 100,
			CacheHitRatio: 0.999},
		Replication: &collector.ReplicationStats{Replicas: []collector.ReplicaInfo{
			{ClientAddr: &addr, ReplayLag: &lag}}},
	}
}

// readCaseIncident reads a sage.incidents row the way the Cases API does and
// builds the projection input from the stored columns only.
func readCaseIncident(t *testing.T, ctx context.Context, signal string) cases.SourceIncident {
	t.Helper()
	pool := requireDB(t)
	var (
		in    cases.SourceIncident
		chain []byte
		sev   string
		sql   *string
		risk  *string
	)
	err := pool.QueryRow(ctx, `SELECT id::text, severity, root_cause, causal_chain,
		    affected_objects, signal_ids, recommended_sql, action_risk, source,
		    confidence::float8, detected_at, last_detected_at, occurrence_count
		FROM sage.incidents WHERE $1 = ANY(signal_ids) AND resolved_at IS NULL`,
		signal).Scan(&in.ID, &sev, &in.RootCause, &chain, &in.AffectedObjects,
		&in.SignalIDs, &sql, &risk, &in.Source, &in.Confidence, &in.DetectedAt,
		&in.LastDetectedAt, &in.OccurrenceCount)
	if err != nil {
		t.Fatalf("read %s incident: %v", signal, err)
	}
	if err := json.Unmarshal(chain, &in.CausalChain); err != nil {
		t.Fatalf("decode causal_chain %s: %v", chain, err)
	}
	in.Severity = cases.Severity(sev)
	if sql != nil {
		in.RecommendedSQL = *sql
	}
	if risk != nil {
		in.ActionRisk = *risk
	}
	return in
}

// Integration (playbooks-02): the engine detects replication lag on a
// fixture snapshot, persists the incident to real PostgreSQL, and the case
// projected from the stored row carries the WAL diagnostic.
func TestPersistedReplicationLagIncidentProjectsWALDiagnostic(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := integConfig()
	cfg.RCA.ReplicationLagThresholdS = 30
	eng := NewEngine(&cfg.RCA, noopLog)
	eng.Analyze(replicationLagSnapshot(), nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}

	stored := readCaseIncident(t, ctx, signalid.ReplicationLagIncreasing)
	if !reflect.DeepEqual(stored.SignalIDs, []string{signalid.ReplicationLagIncreasing}) {
		t.Fatalf("stored signal ids = %v", stored.SignalIDs)
	}
	got := cases.ProjectIncident(stored)
	if types := caseActionTypes(got); !reflect.DeepEqual(types,
		[]string{"diagnose_wal_replication"}) {
		t.Fatalf("projected candidates = %v, want [diagnose_wal_replication]", types)
	}
	c := got.ActionCandidates[0]
	if c.RiskTier != "safe" || c.ProposedSQL == "" || c.BlockedReason != "" {
		t.Fatalf("WAL diagnostic = %+v", c)
	}
	// The diagnostic is a script of probe statements (playbooks-04): each
	// one must plan.
	stmts := strings.Split(strings.TrimSuffix(c.ProposedSQL, ";"), ";\n\n")
	if len(stmts) != 2 {
		t.Fatalf("WAL diagnostic has %d statements, want slots and lag", len(stmts))
	}
	for _, stmt := range stmts {
		var plan string
		if err := pool.QueryRow(ctx, "EXPLAIN "+stmt).Scan(&plan); err != nil {
			t.Fatalf("WAL diagnostic SQL does not plan on PostgreSQL: %v\n%s", err, stmt)
		}
	}
}

// Integration (playbooks-02): the sequence review script the case offers
// for an analyzer sequence_exhaustion finding runs on PostgreSQL and names
// the owning column.
func TestSequenceFindingScriptRunsOnPostgres(t *testing.T) {
	pool := requireDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS public.pb02_orders`,
		`CREATE TABLE public.pb02_orders (id serial PRIMARY KEY, note text)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.pb02_orders`)
	})
	got := cases.ProjectFinding(cases.SourceFinding{ID: "f-seq",
		Category: "sequence_exhaustion", ObjectType: "sequence",
		ObjectIdentifier: "public.pb02_orders_id_seq", ObservedAt: time.Now()})
	if len(got.ActionCandidates) != 1 || got.ActionCandidates[0].ScriptOutput == nil {
		t.Fatalf("sequence finding candidates = %+v", got.ActionCandidates)
	}
	script := got.ActionCandidates[0].ScriptOutput
	if _, err := pool.Exec(ctx, script.MigrationSQL); err != nil {
		t.Fatalf("migration script fails on PostgreSQL: %v\n%s", err, script.MigrationSQL)
	}
	for _, sql := range script.VerificationSQL {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("verification SQL fails: %v\n%s", err, sql)
		}
	}
	owner := strings.SplitN(script.MigrationSQL, ";\n", 2)[0]
	owner = owner[strings.Index(owner, "SELECT"):]
	var table, column, colType string
	owner = "SELECT owning_table::text, owning_column::text, column_type FROM (" +
		strings.TrimSuffix(strings.TrimSpace(owner), ";") + ") q"
	if err := pool.QueryRow(ctx, owner).Scan(&table, &column, &colType); err != nil {
		t.Fatalf("owning-column query: %v\n%s", err, owner)
	}
	if table != "pb02_orders" || column != "id" || colType != "integer" {
		t.Fatalf("owner = %s.%s %s, want pb02_orders.id integer", table, column, colType)
	}
}
