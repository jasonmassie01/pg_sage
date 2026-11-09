package tuner

import "context"

// sage.query_hints status lifecycle (Phase 0 item 11):
//
//	proposed    the tuner recommended the hint; nothing is installed yet
//	active      the hint is installed in hint_plan.hints (applied)
//	dormant     installed, but its query has not run since the last
//	            revalidation (or its queryid is absent); back to active
//	            once calls advance again
//	rolled_back the installed hint was removed again (rollback/operator)
//	broken      set by revalidation when a referenced index is missing
//	retired     set by the retire_query_hint action (cases); revalidation
//	            no longer retires (tuning-01)
//
// The executor installs hints through the approval path; the tuner
// reconciles the status with hint_plan.hints at the start of each cycle.

// upsertQueryHint records a proposed hint for the dashboard query-hints
// page: it updates the query's open proposal, or inserts one. The tuner
// prescribes no query rewrites, so suggested_rewrite and rewrite_rationale
// are written empty (the columns stay for the case query actions).
func (t *Tuner) upsertQueryHint(
	ctx context.Context, queryID int64, hintText, symptom string,
) {
	if t.pool == nil {
		return
	}
	tag, err := t.pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.query_hints
		 SET hint_text = $2, symptom = $3,
		     suggested_rewrite = '', rewrite_rationale = ''
		 WHERE queryid = $1 AND status = 'proposed'`,
		queryID, hintText, symptom,
	)
	if err != nil {
		t.logFn("WARN", "tuner: update proposed query_hint %d: %v", queryID, err)
		return
	}
	if tag.RowsAffected() > 0 {
		return
	}
	_, err = t.pool.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.query_hints
			(queryid, hint_text, symptom,
			 suggested_rewrite, rewrite_rationale, status)
		 VALUES ($1, $2, $3, '', '', 'proposed')`,
		queryID, hintText, symptom,
	)
	if err != nil {
		t.logFn("WARN", "tuner: insert proposed query_hint %d: %v", queryID, err)
	}
}

// hintAppliedSQL marks a proposal active once exactly its hint text is
// installed for the query.
const hintAppliedSQL = `/* pg_sage */ UPDATE sage.query_hints q
   SET status = 'active'
 WHERE q.status = 'proposed'
   AND EXISTS (SELECT 1 FROM hint_plan.hints h
                WHERE h.query_id = q.queryid AND h.application_name = ''
                  AND h.hints = q.hint_text)`

// hintRolledBackSQL marks an active or dormant hint rolled back once its
// own text is no longer installed for the query: a row an operator replaced
// with their own hint no longer carries pg_sage's (tuning-02a).
const hintRolledBackSQL = `/* pg_sage */ UPDATE sage.query_hints q
   SET status = 'rolled_back', rolled_back_at = now()
 WHERE q.status IN ('active', 'dormant')
   AND NOT EXISTS (SELECT 1 FROM hint_plan.hints h
                    WHERE h.query_id = q.queryid AND h.application_name = ''
                      AND h.hints = q.hint_text)`

// reconcileHintStatuses moves query_hints rows to the state hint_plan.hints
// shows. Without a ready hint table nothing can have been applied.
func (t *Tuner) reconcileHintStatuses(ctx context.Context) {
	if h := t.hints(); t.pool == nil || h == nil || !h.HintTableReady {
		return
	}
	for _, step := range []struct{ name, sql string }{
		{"applied", hintAppliedSQL}, {"rolled back", hintRolledBackSQL},
	} {
		tag, err := t.pool.Exec(ctx, step.sql)
		if err != nil {
			t.logFn("WARN", "tuner: reconcile %s query hints: %v", step.name, err)
			continue
		}
		if n := tag.RowsAffected(); n > 0 {
			t.logFn("INFO", "tuner: %d query hint(s) marked %s", n, step.name)
		}
	}
}

// loadFacts starts the cycle's catalog facts empty: cycleFacts reads them
// for the relations each plan names.
func (t *Tuner) loadFacts(context.Context) {
	t.facts = newCatalogFacts()
}

// cycleFacts reads the facts of the relations planJSON names that this
// cycle has not read yet. On error the catalog-dependent heuristics are
// skipped for this plan (nil facts).
func (t *Tuner) cycleFacts(ctx context.Context, planJSON []byte) *CatalogFacts {
	if t.facts == nil {
		t.facts = newCatalogFacts()
	}
	names, err := planRelationNames(planJSON)
	if err == nil {
		err = t.facts.ensure(ctx, t.pool, names)
	}
	if err != nil {
		t.logFn("WARN", "tuner: %v; seq-scan and parallel checks skipped", err)
		return nil
	}
	return t.facts
}
