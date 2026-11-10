package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// clouds-08 fleet isolation on real PostgreSQL: databases A and B share
// one telemetry runtime, yet each one's managed_parameter_drift findings
// are written to its own database from its own worker's cycle, never the
// other's (the fleet "all" leak must not come back through the share).

func driftParameters(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT object_identifier
		FROM sage.findings WHERE category = $1 AND status = 'open'
		ORDER BY object_identifier`, categoryManagedDrift)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestCloudTelemetryShareDriftFindingsStayPerDatabase(t *testing.T) {
	ctx := context.Background()
	poolA := clusterPool(t, ctx, "share_drift_a")
	poolB := clusterPool(t, ctx, "share_drift_b")
	shared := cloudtel.Unavailable("share-drift-a", "rds", "no AWS credentials")
	rtA := &databaseRuntime{spec: databaseRuntimeSpec{Name: "share-drift-a", Pool: poolA,
		Scope: "share-drift-a"}, ctx: ctx, cloud: shared}
	rtB := &databaseRuntime{spec: databaseRuntimeSpec{Name: "share-drift-b", Pool: poolB,
		Scope: "share-drift-b"}, ctx: ctx, cloud: shared}
	rtA.afterManagedCycle(managedparam.CycleResult{Drift: []managedparam.Drift{{
		Parameter: "work_mem", Configured: "8192", Running: "4096",
		Kind: managedparam.DriftMismatch}}}, nil)
	rtB.afterManagedCycle(managedparam.CycleResult{Drift: []managedparam.Drift{{
		Parameter: "max_wal_size", Configured: "4096", Running: "2048",
		Kind: managedparam.DriftMismatch}}}, nil)
	a, b := driftParameters(t, poolA), driftParameters(t, poolB)
	if len(a) != 1 || a[0] != "parameter:work_mem" {
		t.Fatalf("A drift findings = %v, want only its own work_mem", a)
	}
	if len(b) != 1 || b[0] != "parameter:max_wal_size" {
		t.Fatalf("B drift findings = %v, want only its own max_wal_size", b)
	}
}
