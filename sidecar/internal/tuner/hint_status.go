package tuner

import (
	"context"
	"time"
)

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
//	rejected    an operator rejected the approval of the proposal's
//	            query_tuning finding (tuning-06)
//	expired     the proposal waited proposalExpiry without an answer
//	            (tuning-06)
//
// The executor installs hints through the approval path; the tuner
// reconciles the status with hint_plan.hints and the action queue at the
// start of each cycle. Neither rejected nor expired holds the statement:
// it may take a new proposal (one per statement, hintStates).

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
		`/* pg_sage tuner */ UPDATE sage.query_hints
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
		`/* pg_sage tuner */ INSERT INTO sage.query_hints
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
const hintAppliedSQL = `/* pg_sage tuner */ UPDATE sage.query_hints q
   SET status = 'active'
 WHERE q.status = 'proposed'
   AND EXISTS (SELECT 1 FROM hint_plan.hints h
                WHERE h.query_id = q.queryid AND h.application_name = ''
                  AND h.hints = q.hint_text)`

// hintRolledBackSQL marks an active or dormant hint rolled back once its
// own text is no longer installed for the query: a row an operator replaced
// with their own hint no longer carries pg_sage's (tuning-02a).
const hintRolledBackSQL = `/* pg_sage tuner */ UPDATE sage.query_hints q
   SET status = 'rolled_back', rolled_back_at = now()
 WHERE q.status IN ('active', 'dormant')
   AND NOT EXISTS (SELECT 1 FROM hint_plan.hints h
                    WHERE h.query_id = q.queryid AND h.application_name = ''
                      AND h.hints = q.hint_text)`

// proposalExpiry is how long a proposal waits for an answer before it
// expires: the tuning agent's operator rejection window (30 days).
const proposalExpiry = 30 * 24 * time.Hour

// hintRejectedSQL marks a proposal rejected once an operator rejected the
// approval of its statement's query_tuning finding after it was proposed
// (a system expiry has no decided_by and rejects nothing).
const hintRejectedSQL = `/* pg_sage tuner */ UPDATE sage.query_hints q
   SET status = 'rejected'
 WHERE q.status = 'proposed'
   AND EXISTS (SELECT 1 FROM sage.action_queue a
                 JOIN sage.findings f ON f.id = a.finding_id
                WHERE a.status = 'rejected' AND a.decided_by IS NOT NULL
                  AND a.decided_at >= q.created_at
                  AND f.category = 'query_tuning'
                  AND f.object_identifier = 'queryid:' || q.queryid)`

// hintExpiredSQL expires the proposals older than $1 seconds.
const hintExpiredSQL = `/* pg_sage tuner */ UPDATE sage.query_hints
   SET status = 'expired'
 WHERE status = 'proposed'
   AND created_at < now() - make_interval(secs => $1::double precision)`

// reconcileHintStatuses moves query_hints rows to the state hint_plan.hints
// and the action queue show. Without a ready hint table nothing can have
// been applied or rolled back; proposals still expire or are rejected.
func (t *Tuner) reconcileHintStatuses(ctx context.Context) {
	if t.pool == nil {
		return
	}
	if h := t.hints(); h != nil && h.HintTableReady {
		t.reconcileStep(ctx, "applied", hintAppliedSQL)
		t.reconcileStep(ctx, "rolled back", hintRolledBackSQL)
	}
	t.reconcileLifecycle(ctx)
}

// reconcileLifecycle rejects and expires open proposals and returns how
// many of each it moved. Each is one UPDATE of 'proposed' rows, so tuners
// racing on the same row move it once.
func (t *Tuner) reconcileLifecycle(ctx context.Context) (rejected, expired int64) {
	if t.pool == nil {
		return 0, 0
	}
	rejected = t.reconcileStep(ctx, "rejected", hintRejectedSQL)
	expired = t.reconcileStep(ctx, "expired", hintExpiredSQL, proposalExpiry.Seconds())
	return rejected, expired
}

// reconcileStep runs one status UPDATE and returns the rows it moved; a
// failure is logged and moves nothing.
func (t *Tuner) reconcileStep(ctx context.Context, name, sql string, args ...any) int64 {
	tag, err := t.pool.Exec(ctx, sql, args...)
	if err != nil {
		t.logFn("WARN", "tuner: reconcile %s query hints: %v", name, err)
		return 0
	}
	n := tag.RowsAffected()
	if n > 0 {
		t.logFn("INFO", "tuner: %d query hint(s) marked %s", n, name)
	}
	return n
}

// hintRejectedRecentlySQL reports an operator rejection, within $3
// seconds, of exactly this hint text for the statement. The window runs
// from the operator's decision: a proposal answered on its 29th day binds
// for 30 more days, not one. The proposal's own age still counts when the
// decision row is gone (a rejection is never older than its proposal).
const hintRejectedRecentlySQL = `/* pg_sage tuner */
SELECT EXISTS (
    SELECT 1 FROM sage.query_hints q
     WHERE q.queryid = $1 AND q.hint_text = $2 AND q.status = 'rejected'
       AND (q.created_at >= now() - make_interval(secs => $3::double precision)
            OR EXISTS (SELECT 1 FROM sage.action_queue a
                         JOIN sage.findings f ON f.id = a.finding_id
                        WHERE a.status = 'rejected' AND a.decided_by IS NOT NULL
                          AND a.decided_at >= q.created_at
                          AND a.decided_at >= now() - make_interval(
                                  secs => $3::double precision)
                          AND f.category = 'query_tuning'
                          AND f.object_identifier = 'queryid:' || q.queryid)))`

// rejectedHint reports whether an operator rejected hint for the statement
// within proposalExpiry, so the deterministic pass does not propose it
// again. An unreadable answer counts as rejected (no proposal is made).
func (t *Tuner) rejectedHint(ctx context.Context, queryID int64, hint string) bool {
	if t.pool == nil || hint == "" {
		return false
	}
	var rejected bool
	err := t.pool.QueryRow(ctx, hintRejectedRecentlySQL, queryID, hint,
		proposalExpiry.Seconds()).Scan(&rejected)
	if err != nil {
		t.logFn("WARN", "tuner: read rejected hints of queryid %d, proposing none: %v",
			queryID, err)
		return true
	}
	if rejected {
		t.logFn("INFO", "tuner: queryid %d: an operator rejected %s; not proposing "+
			"it again within %s", queryID, hint, proposalExpiry)
	}
	return rejected
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
	names, err := PlanRelationNames(planJSON)
	if err == nil {
		err = t.facts.ensure(ctx, t.pool, names)
	}
	if err != nil {
		t.logFn("WARN", "tuner: %v; seq-scan and parallel checks skipped", err)
		return nil
	}
	return t.facts
}
