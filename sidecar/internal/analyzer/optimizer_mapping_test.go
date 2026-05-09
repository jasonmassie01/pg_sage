package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

func TestOptimizerRecommendationToFinding_SeparatesActionLevelFromRisk(t *testing.T) {
	rec := optimizer.Recommendation{
		Table:                   "public.orders",
		DDL:                     "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)",
		DropDDL:                 "DROP INDEX CONCURRENTLY IF EXISTS idx_orders_status",
		Rationale:               "speed up status lookups",
		Severity:                "warning",
		Confidence:              0.62,
		IndexType:               "btree",
		Category:                "missing_index",
		EstimatedImprovementPct: 25,
		ActionLevel:             "advisory",
		AffectedQueries:         []string{"SELECT * FROM orders WHERE status = $1"},
	}
	result := &optimizer.Result{PlanSource: "pg_stat_statements"}

	finding := optimizerRecommendationToFinding(rec, result)

	if finding.ActionRisk != optimizer.RiskModerate {
		t.Fatalf("ActionRisk = %q, want %q",
			finding.ActionRisk, optimizer.RiskModerate)
	}
	if finding.ActionRisk == rec.ActionLevel {
		t.Fatalf("ActionRisk reused ActionLevel %q", rec.ActionLevel)
	}
	if finding.Detail["action_level"] != rec.ActionLevel {
		t.Fatalf("detail action_level = %v, want %q",
			finding.Detail["action_level"], rec.ActionLevel)
	}
	if finding.RecommendedSQL != rec.DDL {
		t.Fatalf("RecommendedSQL = %q, want %q",
			finding.RecommendedSQL, rec.DDL)
	}
	if finding.RollbackSQL != rec.DropDDL {
		t.Fatalf("RollbackSQL = %q, want %q",
			finding.RollbackSQL, rec.DropDDL)
	}
}
