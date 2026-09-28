//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/verify"
)

// D6 positive path: with fresh pg-side IO evidence and a declared capacity
// (operator attestation), an autonomous index build earns load admission and
// runs unattended through the real pipeline.
func TestPipelineIndexBuildEarnsLoadAdmission(t *testing.T) {
	pool := pipelinePool(t)
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_d6;
		CREATE TABLE shp_d6 (id bigint, v text);
		INSERT INTO shp_d6 SELECT g, g::text FROM generate_series(1,1000) g;`)
	cfg := autonomousConfig()
	cfg.Verify.IOCapacity = &config.IOCapacityConfig{ReadWriteMBps: 1000, WALMBps: 1000}
	an, ex := newPipelineExecutor(t, pool, cfg)

	monitor := verify.NewIOMonitor(pool, "e2e", 14*24*time.Hour)
	ctx := context.Background()
	if err := monitor.Sample(ctx); err != nil {
		t.Fatalf("prime IO sampler: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := monitor.Sample(ctx); err != nil {
		t.Fatalf("sample IO rate: %v", err)
	}
	ex.WithIOEvidence(monitor)

	f := analyzer.Finding{
		Category: "missing_index", Severity: "warning",
		ObjectType: "table", ObjectIdentifier: "public.shp_d6",
		Title:          "missing index on shp_d6(id)",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_d6_id_idx ON public.shp_d6 (id)",
		RollbackSQL:    "DROP INDEX CONCURRENTLY IF EXISTS shp_d6_id_idx",
		ActionRisk:     "moderate",
		Detail:         map[string]any{},
	}
	f.Detail["queryids"] = []int64{pipelineQueryID(t, pool, f.ObjectIdentifier)}
	driveFinding(t, pool, an, ex, f)

	act, ok := latestActionFor(t, pool, "missing_index", "public.shp_d6")
	if !ok || !isExecutedOutcome(act.Outcome) {
		t.Fatalf("admitted build did not execute: %#v exists=%v", act, ok)
	}
	if _, valid, exists := indexAccessMethod(t, pool, "shp_d6_id_idx"); !exists || !valid {
		t.Fatalf("index shp_d6_id_idx exists=%v valid=%v", exists, valid)
	}
	var mode string
	err := pool.QueryRow(ctx, `SELECT COALESCE(evidence->'load_admission'->>'mode', '')
		FROM sage.decision WHERE evidence ? 'load_admission'
		ORDER BY id DESC LIMIT 1`).Scan(&mode)
	if err != nil || mode != "declared_capacity" {
		t.Fatalf("decision evidence mode = %q err=%v, want declared_capacity", mode, err)
	}
}
