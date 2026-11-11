package tuner

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// HintRemovalCategory is the finding category for installed
// hint_plan.hints rows whose sage.query_hints record was retired or
// marked broken.
const HintRemovalCategory = "query_hint_retirement"

// hintRemovalFindings reconciles installed pg_hint_plan rows against
// durable metadata (C11). The revalidator only updates
// sage.query_hints; PostgreSQL keeps applying the installed hint until the
// row is deleted. For every query with a retired/broken sage hint and no
// live (active or dormant) one, whose hint_plan.hints row still holds that
// hint's text, a finding carrying the text-scoped BuildDeleteSQL is emitted
// so removal runs through the executor's retire_query_hint policy path (and
// is retried until the row is gone). Rows whose text pg_sage never recorded,
// such as an operator's later replacement, are left alone (tuning-02a).
func (t *Tuner) hintRemovalFindings(ctx context.Context) []analyzer.Finding {
	if h := t.hints(); t.pool == nil || h == nil || !h.HintTableReady {
		return nil
	}
	rows, err := t.pool.Query(ctx, `/* pg_sage tuner */
SELECT DISTINCT h.query_id, h.hints,
       (SELECT q.status FROM sage.query_hints q
         WHERE q.queryid = h.query_id AND q.hint_text = h.hints
           AND q.status IN ('retired', 'broken')
         ORDER BY q.id DESC LIMIT 1)
FROM hint_plan.hints h
WHERE h.application_name = ''
  AND EXISTS (SELECT 1 FROM sage.query_hints q
               WHERE q.queryid = h.query_id AND q.hint_text = h.hints
                 AND q.status IN ('retired', 'broken'))
  AND NOT EXISTS (SELECT 1 FROM sage.query_hints q
                   WHERE q.queryid = h.query_id
                     AND q.status IN ('active', 'dormant'))
ORDER BY h.query_id`)
	if err != nil {
		t.logFn("WARN", "tuner: reconcile installed hints: %v", err)
		return nil
	}
	defer rows.Close()
	var out []analyzer.Finding
	for rows.Next() {
		var queryID int64
		var hint, status string
		if err := rows.Scan(&queryID, &hint, &status); err != nil {
			t.logFn("WARN", "tuner: scan installed hint: %v", err)
			return out
		}
		out = append(out, hintRemovalFinding(queryID, hint, status))
	}
	if err := rows.Err(); err != nil {
		t.logFn("WARN", "tuner: reconcile installed hints rows: %v", err)
	}
	return out
}

func hintRemovalFinding(queryID int64, hint, status string) analyzer.Finding {
	return analyzer.Finding{
		Category:         HintRemovalCategory,
		Severity:         "info",
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", queryID),
		Title: fmt.Sprintf(
			"Remove %s pg_hint_plan hint for queryid %d", status, queryID),
		Detail: map[string]any{
			"queryid":     queryID,
			"hint_status": status,
			"hint_text":   hint,
		},
		Recommendation: "The hint was " + status + " by revalidation but is " +
			"still installed in hint_plan.hints, so PostgreSQL keeps applying it.",
		RecommendedSQL: BuildDeleteSQL(queryID, hint),
		ActionRisk:     "safe",
	}
}
