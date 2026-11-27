package optimizer

// Admission gates that need the what-if's measurement (indexes-02). The
// tuning agent sends no improvement estimate, so judging write impact
// before HypoPG compared 0% with the minimum and refused every candidate
// on a write-heavy table unmeasured. Only a complete measurement rejects;
// anything less keeps the candidate unverified, for an operator.

// minImprovePct is the improvement a complete measurement must reach on
// tc: hypopg_min_improvement_pct, raised to write_impact_threshold_pct on
// a write-heavy table. Rejection memory suppresses and lists against the
// same minimum, so a write-impact rejection lapses once the table is no
// longer write-heavy.
func (o *Optimizer) minImprovePct(tc TableContext) float64 {
	if o.validator.writeHeavy(tc) {
		return max(o.cfg.HypoPGMinImprovePct, o.validator.writeImpactThreshold())
	}
	return o.cfg.HypoPGMinImprovePct
}

// gateWriteImpact applies the write-impact minimum to a measured candidate
// on a write-heavy table: a verified improvement below it rejects; an
// unverified candidate stays unverified, and its reason says that the
// improvement on a write-heavy table is unmeasured.
func (o *Optimizer) gateWriteImpact(rec Recommendation, tc TableContext,
) (Recommendation, bool) {
	if !o.validator.writeHeavy(tc) {
		return rec, false
	}
	if rec.WhatIf != WhatIfVerified {
		rec.WhatIfReason = joinReasons(rec.WhatIfReason,
			"write-heavy table; improvement unmeasured")
		return rec, false
	}
	if ok, reason := o.validator.checkWriteImpact(rec, tc); !ok {
		rec.WhatIf, rec.WhatIfReason, rec.Validated = WhatIfRejected, reason, false
		return rec, true
	}
	return rec, false
}

// noteUnknownColumns says, on an unverified candidate, that its columns
// were not checked this cycle. A verified one needs no note: HypoPG
// planned the index, so its columns exist.
func noteUnknownColumns(rec Recommendation, tc TableContext) Recommendation {
	if !tc.ColumnsKnown && rec.WhatIf != WhatIfVerified {
		rec.WhatIfReason = joinReasons(rec.WhatIfReason,
			"column metadata unavailable this cycle")
	}
	return rec
}

func joinReasons(reason, more string) string {
	if reason == "" {
		return more
	}
	return reason + "; " + more
}
