package analyzer

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// optimizerRecommendationToFinding maps one optimizer candidate to a
// finding. Identity is "schema.table|<normalized index definition>" with
// the fixed optimizer category, so several candidates for one table
// persist independently and the optimizer's open-finding lookup matches
// exactly what is emitted (C05, C06, G2-B19, G3-B13). The table is kept
// separately in Detail["table"]; use OptimizerFindingTable to read it.
func optimizerRecommendationToFinding(
	rec optimizer.Recommendation,
	result *optimizer.Result,
) Finding {
	planSource := ""
	if result != nil {
		planSource = result.PlanSource
	}
	indexCategory := rec.IndexCategory
	if indexCategory == "" {
		indexCategory = rec.Category
	}
	ident := rec.FindingIdentifier()
	detail := map[string]any{
		"table":                     rec.Table,
		"index_fingerprint":         strings.TrimPrefix(ident, rec.Table+"|"),
		"ddl":                       rec.DDL,
		"drop_ddl":                  rec.DropDDL,
		"llm_rationale":             rec.Rationale,
		"confidence_score":          rec.Confidence,
		"action_level":              rec.ActionLevel,
		"action_risk":               optimizer.RiskTierForRecommendation(rec),
		"index_type":                rec.IndexType,
		"category":                  optimizer.OptimizerCategory,
		"index_category":            indexCategory,
		"estimated_improvement_pct": rec.EstimatedImprovementPct,
		"hypopg_validated":          rec.Validated,
		"plan_source":               planSource,
		"affected_queries":          rec.AffectedQueries,
		"queryids":                  rec.AffectedQueryIDs,
	}
	f := Finding{
		Category:         optimizer.OptimizerCategory,
		Severity:         rec.Severity,
		ObjectType:       "index",
		ObjectIdentifier: ident,
		Title:            fmt.Sprintf("Index recommendation for %s", rec.Table),
		Detail:           detail,
		Recommendation:   rec.Rationale,
		RecommendedSQL:   rec.DDL,
		RollbackSQL:      rec.DropDDL,
		ActionRisk:       optimizer.RiskTierForRecommendation(rec),
	}
	addWhatIfDetail(&f, rec)
	if rec.PartitionedParent {
		advisePartitionPlan(&f, rec)
	}
	return f
}

// addWhatIfDetail records the HypoPG evidence and the verdict the executor
// gates on: only a "verified" index may run without approval (Phase 0
// item 7).
func addWhatIfDetail(f *Finding, rec optimizer.Recommendation) {
	if rec.CostEstimate != nil && rec.CostEstimate.EstimatedSizeBytes > 0 {
		f.Detail["estimated_size_bytes"] = rec.CostEstimate.EstimatedSizeBytes
	}
	verdict := rec.WhatIf
	if verdict == "" {
		verdict = optimizer.WhatIfUnverified
	}
	f.Detail["what_if_verdict"] = verdict
	if rec.WhatIfReason != "" {
		f.Detail["what_if_reason"] = rec.WhatIfReason
	}
}

// advisePartitionPlan makes an index on a partitioned table advisory:
// PostgreSQL rejects CREATE INDEX CONCURRENTLY on the parent, so there is
// no single executable statement; the plan is in the detail.
func advisePartitionPlan(f *Finding, rec optimizer.Recommendation) {
	f.RecommendedSQL, f.RollbackSQL = "", ""
	f.Detail["partitioned_parent"] = true
	note := "This table is partitioned, so the index cannot be built " +
		"CONCURRENTLY on the parent. "
	if len(rec.PartitionPlan) > 0 {
		f.Detail["partition_plan"] = rec.PartitionPlan
		note += "Run partition_plan in order: the parent index ON ONLY, then each " +
			"partition's index CONCURRENTLY and ATTACH PARTITION."
	} else {
		note += "It has sub-partitioned partitions: build the index per leaf " +
			"partition and attach each level."
	}
	f.Recommendation = strings.TrimSpace(rec.Rationale + "\n\n" + note)
}

// OptimizerFindingTable returns the "schema.table" an optimizer finding
// targets: Detail["table"] when present, otherwise the identifier prefix
// before '|' (legacy findings used the bare table as identifier).
func OptimizerFindingTable(f Finding) string {
	if t, ok := f.Detail["table"].(string); ok && t != "" {
		return t
	}
	table, _, _ := strings.Cut(f.ObjectIdentifier, "|")
	return table
}
