package executor

import (
	"slices"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// rules-13: the index_bloat rule's statement runs under the existing
// reindex_concurrently contract (moderate, approval required), and its
// case offers the rebuild for approval or as a script, never unattended.
//
// No concurrent access tests: classification and projection are pure.

func indexBloatFinding(t *testing.T) analyzer.Finding {
	t.Helper()
	snap := &collector.Snapshot{IndexBloat: []collector.IndexBloat{{SchemaName: "app",
		RelName: "orders", IndexRelName: "orders_created_idx", RelPages: 30000,
		EstimatedPages: 9000, IndexBytes: 30000 * 8192, BloatBytes: 21000 * 8192,
		BloatPct: 70, Fillfactor: 90, Reltuples: 2e6}}}
	cfg := &config.Config{Analyzer: config.AnalyzerConfig{IndexBloatThresholdPct: 30}}
	for _, rule := range analyzer.AllRules {
		if rule.Name == "index_bloat" {
			got := rule.Fn(snap, nil, cfg, nil)
			if len(got) != 1 {
				t.Fatalf("index_bloat findings = %+v, want one", got)
			}
			return got[0]
		}
	}
	t.Fatal("no index_bloat rule registered")
	return analyzer.Finding{}
}

func TestIndexBloat_RunsUnderTheReindexContract(t *testing.T) {
	f := indexBloatFinding(t)
	actionType, riskTier := ClassifySQL(f.RecommendedSQL)
	if actionType != "reindex_concurrently" || riskTier != "moderate" {
		t.Fatalf("ClassifySQL(%q) = %s/%s, want reindex_concurrently/moderate",
			f.RecommendedSQL, actionType, riskTier)
	}
	contract, ok := ContractForActionType(actionType)
	if !ok || contract.BaseRiskTier != "moderate" ||
		!slices.ContainsFunc(contract.Guardrails, isApprovalRequiredGuardrail) {
		t.Fatalf("contract = %+v (found %v), want moderate with approval required",
			contract, ok)
	}
	if f.ActionRisk != contract.BaseRiskTier {
		t.Fatalf("finding ActionRisk %q differs from the contract tier %q",
			f.ActionRisk, contract.BaseRiskTier)
	}
}

func TestIndexBloat_CaseOffersApprovalOrScriptOnly(t *testing.T) {
	f := indexBloatFinding(t)
	c := cases.ProjectFinding(cases.SourceFinding{ID: "f1", Category: f.Category,
		Severity: cases.Severity(f.Severity), ObjectType: f.ObjectType,
		ObjectIdentifier: f.ObjectIdentifier, Title: f.Title, Detail: f.Detail, Recommendation: f.Recommendation,
		RecommendedSQL: f.RecommendedSQL}, ClassifySQL)
	if len(c.ActionCandidates) != 1 {
		t.Fatalf("candidates = %+v, want one", c.ActionCandidates)
	}
	got := c.ActionCandidates[0]
	if got.ActionType != "reindex_concurrently" || got.RiskTier != "moderate" ||
		got.ProposedSQL != f.RecommendedSQL || slices.Contains(got.OutputModes, "execute") ||
		!slices.Contains(got.OutputModes, "queue_for_approval") {
		t.Fatalf("candidate = %+v, want a moderate reindex_concurrently for approval", got)
	}
	if !slices.Contains(got.VerificationPlan, "compare index size before and after reindex") {
		t.Fatalf("verification plan = %v, want the reindex size check", got.VerificationPlan)
	}
}
