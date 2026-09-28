package analyzer

// cycleEval tracks which finding categories were fully evaluated in one
// analyzer cycle. Only evaluated categories may resolve their open
// findings; a category whose evaluator was skipped, lacked input or
// failed is left untouched (G2-B02/C03).
type cycleEval struct {
	ok     map[string]bool
	failed map[string]bool
}

func newCycleEval() *cycleEval {
	return &cycleEval{ok: map[string]bool{}, failed: map[string]bool{}}
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
