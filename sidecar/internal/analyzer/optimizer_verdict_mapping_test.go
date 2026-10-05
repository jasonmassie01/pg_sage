package analyzer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Phase 0 item 7: the finding carries the what-if verdict the executor
// gates on, and a partitioned parent's recommendation is advisory (no
// executable SQL; the per-partition plan is in the detail).

func TestOptimizerMapping_CarriesWhatIfVerdict(t *testing.T) {
	rec := optimizer.Recommendation{Table: "public.orders",
		DDL:     "CREATE INDEX CONCURRENTLY idx_o ON public.orders (status)",
		DropDDL: "DROP INDEX CONCURRENTLY IF EXISTS \"public\".\"idx_o\"",
		WhatIf:  optimizer.WhatIfUnverified, WhatIfReason: "hypopg unavailable"}
	f := OptimizerRecommendationFinding(rec, "")
	if f.Detail["what_if_verdict"] != "unverified" ||
		f.Detail["what_if_reason"] != "hypopg unavailable" ||
		f.Detail["hypopg_validated"] != false {
		t.Fatalf("detail = %+v", f.Detail)
	}
	if f.RecommendedSQL != rec.DDL || f.RollbackSQL != rec.DropDDL {
		t.Fatalf("unverified recommendation lost its SQL: %+v", f)
	}
	rec.WhatIf, rec.WhatIfReason, rec.Validated = optimizer.WhatIfVerified, "", true
	f = OptimizerRecommendationFinding(rec, "")
	if f.Detail["what_if_verdict"] != "verified" || f.Detail["hypopg_validated"] != true {
		t.Fatalf("verified detail = %+v", f.Detail)
	}
	if _, ok := f.Detail["what_if_reason"]; ok {
		t.Fatalf("empty reason recorded: %+v", f.Detail)
	}
}

func TestOptimizerMapping_PartitionedParentIsAdvisory(t *testing.T) {
	plan := []string{`CREATE INDEX IF NOT EXISTS "i" ON ONLY "public"."events" ...`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS "i_x" ON "public"."events_p1" ...`,
		`ALTER INDEX "public"."i" ATTACH PARTITION "public"."i_x"`}
	rec := optimizer.Recommendation{Table: "public.events", Rationale: "why",
		DDL:               "CREATE INDEX CONCURRENTLY i ON public.events (status)",
		DropDDL:           "DROP INDEX CONCURRENTLY IF EXISTS \"public\".\"i\"",
		PartitionedParent: true, PartitionPlan: plan, WhatIf: optimizer.WhatIfUnverified}
	f := OptimizerRecommendationFinding(rec, "")
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("partitioned parent kept executable SQL: %q / %q", f.RecommendedSQL,
			f.RollbackSQL)
	}
	if f.Detail["partitioned_parent"] != true ||
		!reflect.DeepEqual(f.Detail["partition_plan"], plan) {
		t.Fatalf("detail = %+v", f.Detail)
	}
	if !strings.Contains(f.Recommendation, "partition") {
		t.Fatalf("recommendation does not explain the partition plan: %q", f.Recommendation)
	}
	if f.ObjectIdentifier != rec.FindingIdentifier() {
		t.Fatalf("identity changed: %q", f.ObjectIdentifier)
	}
}
