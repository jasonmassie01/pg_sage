package tuner

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// hintOwnership is the durable hint state of one statement (tuning-02): one
// hint per statement is enforced from sage.query_hints and hint_plan.hints
// on every check, not from the in-memory cooldown map.
type hintOwnership int

const (
	// hintNone: no open pg_sage proposal and nothing installed.
	hintNone hintOwnership = iota
	// hintProposed: pg_sage has an open proposal; nothing is installed.
	hintProposed
	// hintInstalled: pg_sage's own hint is installed (or recorded active or
	// dormant), including a retired or broken one awaiting removal.
	hintInstalled
	// hintForeign: hint_plan.hints holds a row for the statement whose text
	// no pg_sage record carries, such as an operator's own hint.
	hintForeign
)

// hintStateSQL reads the state of each queryid in $1 from sage.query_hints
// and hint_plan.hints (both indexed on the queryid). A row counts as
// pg_sage's only for the empty application_name (the only one pg_sage
// writes) and text pg_sage recorded for the statement; any other row is
// foreign.
const hintStateSQL = `/* pg_sage tuner */
SELECT ids.qid,
       EXISTS (SELECT 1 FROM hint_plan.hints h
                WHERE h.query_id = ids.qid
                  AND NOT (h.application_name = '' AND EXISTS (
                      SELECT 1 FROM sage.query_hints q
                       WHERE q.queryid = h.query_id AND q.hint_text = h.hints))),
       EXISTS (SELECT 1 FROM hint_plan.hints h WHERE h.query_id = ids.qid)
         OR EXISTS (SELECT 1 FROM sage.query_hints q WHERE q.queryid = ids.qid
                     AND q.status IN ('active', 'dormant')),
       EXISTS (SELECT 1 FROM sage.query_hints q WHERE q.queryid = ids.qid
                AND q.status = 'proposed')
FROM unnest($1::bigint[]) AS ids(qid)`

// hintStateNoTableSQL is hintStateSQL without a hint table: nothing can be
// installed, so only pg_sage's own records count.
const hintStateNoTableSQL = `/* pg_sage tuner */
SELECT ids.qid, false,
       EXISTS (SELECT 1 FROM sage.query_hints q WHERE q.queryid = ids.qid
                AND q.status IN ('active', 'dormant')),
       EXISTS (SELECT 1 FROM sage.query_hints q WHERE q.queryid = ids.qid
                AND q.status = 'proposed')
FROM unnest($1::bigint[]) AS ids(qid)`

// hintStates returns the durable hint state of every queryid in ids that
// has one (absent: hintNone). With no pool or no ids it reads nothing; the
// hint table is read only when it was detected ready.
func (t *Tuner) hintStates(ctx context.Context, ids []int64) (map[int64]hintOwnership, error) {
	out := map[int64]hintOwnership{}
	if t.pool == nil || len(ids) == 0 {
		return out, nil
	}
	query := hintStateNoTableSQL
	if h := t.hints(); h != nil && h.HintTableReady {
		query = hintStateSQL
	}
	rows, err := t.pool.Query(ctx, query, ids)
	if err != nil {
		return nil, fmt.Errorf("read hint state from sage.query_hints and "+
			"hint_plan.hints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var qid int64
		var foreign, installed, proposed bool
		if err := rows.Scan(&qid, &foreign, &installed, &proposed); err != nil {
			return nil, fmt.Errorf("scan hint state: %w", err)
		}
		switch {
		case foreign:
			out[qid] = hintForeign
		case installed:
			out[qid] = hintInstalled
		case proposed:
			out[qid] = hintProposed
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read hint state rows: %w", err)
	}
	return out, nil
}

// candidateIDs lists the queryids of candidates.
func candidateIDs(candidates []candidate) []int64 {
	ids := make([]int64, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.QueryID)
	}
	return ids
}

// operatorHintFinding is the informational finding for a statement pg_sage
// would hint while hint_plan.hints holds a hint it did not install. It
// carries no SQL: pg_sage never replaces or removes an operator's hint.
func operatorHintFinding(
	c candidate, symptoms []PlanSymptom, hint, rationale string,
) analyzer.Finding {
	return analyzer.Finding{
		Category:         "query_tuning",
		Severity:         "info",
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", c.QueryID),
		Title:            fmt.Sprintf("Plan hint for queryid %d: operator hint present", c.QueryID),
		Detail: map[string]any{
			"queryid":               c.QueryID,
			"query":                 c.Query,
			"symptoms":              symptomNames(symptoms),
			"suggested_hint":        hint,
			"operator_hint_present": true,
		},
		Recommendation: "hint_plan.hints already holds a hint for this statement that " +
			"pg_sage did not install, so pg_sage proposes none. It would suggest " +
			hint + " (" + rationale + "); remove or change the installed hint by " +
			"hand if that is wanted.",
		ActionRisk: "safe",
	}
}
