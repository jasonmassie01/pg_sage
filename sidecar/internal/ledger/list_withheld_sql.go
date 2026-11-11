package ledger

// withheldSQL reads open withheld rows through idx_decision_open_withheld
// (its predicate is the WHERE clause's first two terms), groups them by
// candidate (candidate_key; rows written before policy-02 by fingerprint;
// a row with neither stands alone) and keeps each group's latest row with
// the group's repeats. Filters apply to that latest row. $5 is one past
// the limit, so the caller can say the list was cut. No SQL text is read.
//
// An execute resolves only its candidate's fingerprinted rows (policy-02).
// A withheld row recorded without a fingerprint (a lease conflict's park,
// a custodian escalation, a schema-guard verdict) stays unresolved, so it
// is left out once an execute of its candidate was recorded after it: the
// change has run. Execute rows are never resolved, so the anti-join reads
// idx_decision_candidate_open.
const withheldSQL = `/* pg_sage */ WITH withheld AS (
	SELECT d.id, d.verdict, d.reason, d.risk_tier, d.feature, d.intent, d.target_objects,
	       d.repeat_count, d.created_at, COALESCE(d.last_seen_at, d.created_at) AS seen_at,
	       COALESCE(d.candidate_key, d.fingerprint, 'decision:' || d.id) AS grp,
	       d.evidence->>'budget_detail' AS budget_detail,
	       d.evidence->'verification_wait' AS verification_wait,
	       d.evidence->>'verification_wait_detail' AS verification_wait_detail,
	       d.deadline_kind, d.deadline_hard_at, d.policy_version, d.policy_doc_sha256,
	       d.queue_id, d.evidence->>'finding_id' AS finding_id, d.principal_id, d.evidence_id
	  FROM sage.decision d
	 WHERE d.resolved_at IS NULL AND d.verdict <> 'execute'
	   AND ($1::bigint IS NULL OR d.database_id = $1)
	   AND (d.fingerprint IS NOT NULL OR d.candidate_key IS NULL OR NOT EXISTS (
	        SELECT 1 FROM sage.decision e
	         WHERE e.candidate_key = d.candidate_key AND e.resolved_at IS NULL
	           AND e.verdict = 'execute' AND e.id > d.id))
), ranked AS (
	SELECT w.*, sum(w.repeat_count) OVER candidate AS repeats,
	       count(*) OVER candidate AS open_rows, min(w.created_at) OVER candidate AS first_seen,
	       row_number() OVER (PARTITION BY w.grp ORDER BY w.seen_at DESC, w.id DESC) AS latest
	  FROM withheld w
	WINDOW candidate AS (PARTITION BY w.grp)
)
SELECT id, grp, verdict, reason, risk_tier, feature, intent, target_objects, repeats,
       open_rows, first_seen, seen_at, budget_detail, verification_wait,
       verification_wait_detail, deadline_kind, deadline_hard_at, policy_version,
       policy_doc_sha256, queue_id, finding_id, principal_id, evidence_id
  FROM ranked
 WHERE latest = 1 AND ($2::text = '' OR verdict = $2) AND ($3::text = '' OR reason = $3)
   AND ($4::timestamptz IS NULL OR seen_at >= $4)
 ORDER BY seen_at DESC, id DESC
 LIMIT $5`

// withheldQuery is the read and its parameters for filter.
func withheldQuery(filter WithheldFilter) (string, []any) {
	return withheldSQL, []any{filter.DatabaseID, string(filter.Verdict), filter.Reason,
		filter.Since, filter.Limit + 1}
}
