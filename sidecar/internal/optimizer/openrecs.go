package optimizer

import (
	"context"
	"encoding/json"
	"strings"
)

// openRecommendations loads the table's open optimizer findings. They are
// matched on the exact emitted identity scheme — category
// OptimizerCategory and object_identifier "schema.table|<fingerprint>"
// (or the legacy bare "schema.table") — with no LIKE wildcards (C06,
// G3-B12). hasOpen reports whether any exists, which suppresses a new
// LLM call; the returned recommendations are the still-valid, not yet
// acted-on candidates to re-emit so the analyzer does not resolve them.
// The rows are read before any is reloaded: a reload may run a what-if
// session, which needs a connection of its own.
func (o *Optimizer) openRecommendations(
	ctx context.Context, tc TableContext,
) (recs []Recommendation, hasOpen bool) {
	if o.pool == nil {
		return nil, false
	}
	stored, hasOpen := o.loadOpenFindings(ctx, tc.Schema+"."+tc.Table)
	for _, f := range stored {
		if rec, ok := o.reloadRecommendation(ctx, f.detail, tc); ok {
			rec.Severity = f.severity
			recs = append(recs, rec)
		}
	}
	return recs, hasOpen
}

// openFinding is one stored, not yet acted-on optimizer finding.
type openFinding struct{ detail, severity string }

// loadOpenFindings reads the table's open optimizer findings; hasOpen
// counts acted-on ones too.
func (o *Optimizer) loadOpenFindings(ctx context.Context, table string) (
	[]openFinding, bool) {
	rows, err := o.pool.Query(ctx, `/* pg_sage */
		SELECT detail::text, severity, acted_on_at IS NOT NULL
		FROM sage.findings
		WHERE category = $1 AND status = 'open'
		  AND (object_identifier = $2
		       OR left(object_identifier, length($2) + 1) = $2 || '|')
		ORDER BY id`, OptimizerCategory, table)
	if err != nil {
		o.logFn("optimizer", "open recommendations query failed for %s: %v", table, err)
		return nil, false
	}
	defer rows.Close()
	var out []openFinding
	hasOpen := false
	for rows.Next() {
		var f openFinding
		var acted bool
		if err := rows.Scan(&f.detail, &f.severity, &acted); err != nil {
			o.logFn("optimizer", "scan open recommendation for %s: %v", table, err)
			return nil, hasOpen
		}
		hasOpen = true
		if !acted {
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		o.logFn("optimizer", "open recommendations rows for %s: %v", table, err)
	}
	return out, hasOpen
}

// persistedRec mirrors the Detail keys written by the analyzer mapping.
type persistedRec struct {
	DDL          string   `json:"ddl"`
	Rationale    string   `json:"llm_rationale"`
	Confidence   float64  `json:"confidence_score"`
	ActionLevel  string   `json:"action_level"`
	IndexType    string   `json:"index_type"`
	IndexCat     string   `json:"index_category"`
	Improvement  float64  `json:"estimated_improvement_pct"`
	Validated    bool     `json:"hypopg_validated"`
	WhatIf       string   `json:"what_if_verdict"`
	WhatIfReason string   `json:"what_if_reason"`
	Affected     []string `json:"affected_queries"`
	QueryIDs     []int64  `json:"queryids"`
}

// reloadRecommendation rebuilds a stored recommendation and re-runs the
// deterministic gates against the current table context, so a candidate
// whose columns vanished or that an existing index now duplicates is not
// re-emitted (and is therefore resolved by the analyzer).
func (o *Optimizer) reloadRecommendation(
	ctx context.Context, detail string, tc TableContext,
) (Recommendation, bool) {
	var p persistedRec
	if err := json.Unmarshal([]byte(detail), &p); err != nil ||
		strings.TrimSpace(p.DDL) == "" {
		return Recommendation{}, false
	}
	rec := Recommendation{
		DDL: p.DDL, Rationale: p.Rationale, Confidence: p.Confidence,
		ActionLevel: p.ActionLevel, IndexType: p.IndexType,
		IndexCategory: p.IndexCat, EstimatedImprovementPct: p.Improvement,
		AffectedQueries: p.Affected, AffectedQueryIDs: p.QueryIDs,
	}
	rec.WhatIf, rec.WhatIfReason = reloadedVerdict(p)
	rec.Validated = rec.WhatIf == WhatIfVerified
	rec, err := canonicalizeRecommendation(rec, tc)
	if err != nil {
		return rec, false
	}
	if o.validator != nil {
		if ok, _ := o.validator.Validate(ctx, rec, tc); !ok {
			return rec, false
		}
	}
	if rec.WhatIf != WhatIfVerified {
		return o.reverify(ctx, rec, tc)
	}
	return rec, true
}

// reverify re-runs the what-if check on a reloaded unverified candidate
// once HypoPG can measure it (it was missing, or an older release never
// evaluated the candidate), so it becomes verified and may act
// autonomously, or is rejected and not re-emitted (the analyzer then
// resolves it). Without HypoPG the stored verdict and reason stand. A
// verified candidate is never re-evaluated here.
func (o *Optimizer) reverify(
	ctx context.Context, rec Recommendation, tc TableContext,
) (Recommendation, bool) {
	if o.whatIf == nil || !o.whatIf.IsAvailable(ctx) {
		return rec, true
	}
	checked, rejected := o.enrichWithHypoPG(ctx, rec, tc)
	switch {
	case rejected:
		o.logFn("optimizer", "dropping open %s on %s: %s",
			rec.DDL, tc.Schema+"."+tc.Table, checked.WhatIfReason)
		return checked, false
	case checked.WhatIf == WhatIfVerified:
		o.logFn("optimizer", "open %s on %s is now verified by HypoPG (%.0f%% better)",
			rec.DDL, tc.Schema+"."+tc.Table, checked.EstimatedImprovementPct)
	}
	return checked, true
}

// reloadedVerdict restores a stored verdict. Verified requires both the
// verdict and HypoPG's validation flag; a legacy row without a verdict is
// verified only if HypoPG validated it. Anything else is unverified.
func reloadedVerdict(p persistedRec) (string, string) {
	switch {
	case p.Validated && (p.WhatIf == WhatIfVerified || p.WhatIf == ""):
		return WhatIfVerified, ""
	case p.WhatIfReason != "":
		return WhatIfUnverified, p.WhatIfReason
	default:
		return WhatIfUnverified, "no verified what-if evaluation recorded"
	}
}
