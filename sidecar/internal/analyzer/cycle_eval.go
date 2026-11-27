package analyzer

import "time"

// cycleEval tracks which finding categories were fully evaluated in one
// analyzer cycle. Only evaluated categories may resolve their open
// findings; a category whose evaluator was skipped, lacked input or
// failed is left untouched (G2-B02/C03).
type cycleEval struct {
	ok     map[string]bool
	failed map[string]bool
	// keepOpen names, per category, identities whose open finding must
	// not resolve this cycle although it was not emitted: its absence is
	// not yet evidence (an unused_index clock that restarted without
	// evidence after a sidecar restart, rules-04).
	keepOpen map[string]map[string]bool
	// collectedAt is when this cycle's snapshot was collected: the
	// evidence time findings are judged by after an action (rules-06).
	collectedAt time.Time
	// seqHeavy are this cycle's seq_scan_heavy tables (quoted, most
	// sequential scans first), the index floor's work (indexes-12).
	seqHeavy []string
	// disabled and disabledRules are this cycle's rule pack (market-11):
	// disabled categories never resolve as cleared; resolveDisabled
	// resolves them as rule_disabled by disabledBy.
	disabled      map[string]bool
	disabledRules map[string]bool
	disabledBy    string
	packLoaded    bool
}

// evidenceAt returns when this cycle's evidence was collected, zero when
// unknown.
func (e *cycleEval) evidenceAt() time.Time {
	if e == nil {
		return time.Time{}
	}
	return e.collectedAt
}

func newCycleEval() *cycleEval {
	return &cycleEval{ok: map[string]bool{}, failed: map[string]bool{},
		keepOpen: map[string]map[string]bool{}}
}

// keep marks identities of category whose open findings stay open this
// cycle even when the category resolves.
func (e *cycleEval) keep(category string, idents ...string) {
	if e == nil {
		return
	}
	if e.keepOpen[category] == nil {
		e.keepOpen[category] = map[string]bool{}
	}
	for _, id := range idents {
		e.keepOpen[category][id] = true
	}
}

// kept returns the identities of category kept open this cycle.
func (e *cycleEval) kept(category string) map[string]bool {
	if e == nil {
		return nil
	}
	return e.keepOpen[category]
}

// evaluated marks categories as evaluated successfully this cycle.
func (e *cycleEval) evaluated(categories ...string) {
	if e == nil {
		return
	}
	for _, c := range categories {
		e.ok[c] = true
	}
}

// fail marks categories whose evaluation failed or was skipped. A failed
// mark always wins over a success mark for the same category.
func (e *cycleEval) fail(categories ...string) {
	if e == nil {
		return
	}
	for _, c := range categories {
		e.failed[c] = true
	}
}

// hasFailed reports whether category failed this cycle.
func (e *cycleEval) hasFailed(category string) bool {
	return e != nil && e.failed[category]
}

// resolvable returns the categories whose open findings may be resolved:
// every evaluated category plus every emitted category, minus any
// category that failed anywhere this cycle.
func (e *cycleEval) resolvable(findings []Finding) map[string]bool {
	out := make(map[string]bool, len(e.ok)+len(findings))
	for c := range e.ok {
		out[c] = true
	}
	for _, f := range findings {
		out[f.Category] = true
	}
	for c := range e.failed {
		delete(out, c)
	}
	for c := range e.disabled {
		delete(out, c)
	}
	return out
}

// evalFail records a failed evaluation on the current cycle, if any.
// Check helpers call it on query errors; outside a cycle it is a no-op.
func (a *Analyzer) evalFail(categories ...string) {
	a.eval.fail(categories...)
}

// EvaluatedCategoryReporter is implemented by producers (such as the
// forecaster) that can report which categories their latest run fully
// evaluated, so an empty result resolves stale findings.
type EvaluatedCategoryReporter interface {
	LastEvaluatedCategories() []string
}

// IncompleteCategoryReporter is implemented by producers whose latest run
// emitted findings of categories it did not fully evaluate, such as an
// agent posture detector that read only the first rows of a large
// catalog. Those categories are marked failed for the cycle, so the open
// findings the run did not reach stay open.
type IncompleteCategoryReporter interface {
	LastIncompleteCategories() []string
}
