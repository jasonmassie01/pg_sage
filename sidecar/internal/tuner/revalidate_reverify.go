package tuner

import "context"

// reverifyHint is revalidation's counterfactual (tuning-01, deferred to
// tuning-05): a live hint due for re-verification (reverify_due) whose
// statement still has its text in pg_stat_statements is planned plain and
// hinted. Both costs are recorded (before_cost, after_cost, verified_at); a
// no_effect verdict retires the hint, and the tuner's removal finding then
// takes it out of hint_plan.hints. Any other verdict, an unverifiable
// comparison or a failed read leaves the decision as it was: a hint is never
// retired for helping, for costing more, or for being old.
func (t *Tuner) reverifyHint(ctx context.Context, h hintRow, d hintDecision) hintDecision {
	due := h.ReverifyDue != nil || d.reverifyDue
	live := d.action == hintActionKeep || d.action == hintActionDormant
	if !due || !live || !t.HintComparisonEnabled() || h.HintText == "" {
		return d
	}
	query, err := t.statementText(ctx, h.QueryID)
	if err != nil {
		t.logFn("WARN", "revalidate: hint %d not re-verified, statement text: %v", h.ID, err)
		return d
	}
	if query == "" {
		return d
	}
	cmp, _ := t.compareHint(ctx, h.QueryID, query, h.HintText, nil)
	t.recordHintCosts(ctx, h.ID, cmp)
	if cmp.Verdict == HintNoEffect {
		d.action, d.reason = hintActionRetire, "no_effect: "+cmp.Reason
	}
	return d
}

// statementTextSQL reads one text of the queryid in this database ("" when
// absent).
const statementTextSQL = `/* pg_sage tuner */
SELECT coalesce((SELECT query FROM pg_stat_statements
  WHERE queryid = $1
    AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  LIMIT 1), '')`

func (t *Tuner) statementText(ctx context.Context, queryID int64) (string, error) {
	var q string
	err := t.pool.QueryRow(ctx, statementTextSQL, queryID).Scan(&q)
	return q, err
}

// recordHintCosts writes the comparison's costs to the hint's row; a
// comparison that planned nothing writes nothing.
func (t *Tuner) recordHintCosts(ctx context.Context, id int64, cmp HintComparison) {
	if cmp.PlainHash == "" {
		return
	}
	_, err := t.pool.Exec(ctx, `/* pg_sage tuner */
UPDATE sage.query_hints SET before_cost = $2, after_cost = $3, verified_at = now()
WHERE id = $1`, id, cmp.PlainCost, cmp.HintedCost)
	if err != nil {
		t.logFn("WARN", "revalidate: record hint %d costs: %v", id, err)
	}
}
