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

// An optimizer CREATE INDEX that HypoPG did not verify needs approval even
// at full autonomy with load evidence admitted: it is never built on its own.
func TestPipelineUnverifiedIndexNeedsApproval(t *testing.T) {
	pool := pipelinePool(t)
	mustExec(t, pool, `DROP TABLE IF EXISTS shp_w1;
		CREATE TABLE shp_w1 (id bigint, v text);
		INSERT INTO shp_w1 SELECT g, g::text FROM generate_series(1,1000) g;`)
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
	for _, verdict := range []string{"", "unverified"} {
		f := analyzer.Finding{
			Category: "missing_index", Severity: "warning",
			ObjectType: "table", ObjectIdentifier: "public.shp_w1",
			Title:          "missing index on shp_w1(id) " + verdict,
			RecommendedSQL: "CREATE INDEX CONCURRENTLY shp_w1_id_idx ON public.shp_w1 (id)",
			RollbackSQL:    "DROP INDEX CONCURRENTLY IF EXISTS shp_w1_id_idx",
			ActionRisk:     "moderate",
			Detail:         map[string]any{},
		}
		if verdict != "" {
			f.Detail["what_if_verdict"] = verdict
		}
		f.Detail["queryids"] = []int64{pipelineQueryID(t, pool, f.ObjectIdentifier)}
		driveFinding(t, pool, an, ex, f)
		if act, ok := latestActionFor(t, pool, "missing_index", "public.shp_w1"); ok &&
			isExecutedOutcome(act.Outcome) {
			t.Fatalf("verdict %q: unverified index built autonomously: %#v", verdict, act)
		}
		if _, _, exists := indexAccessMethod(t, pool, "shp_w1_id_idx"); exists {
			t.Fatalf("verdict %q: index shp_w1_id_idx exists", verdict)
		}
	}
}
