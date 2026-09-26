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
// row is deleted. For every query whose sage hints are all retired/broken
// but whose hint_plan.hints row still exists, a finding carrying
// BuildDeleteSQL is emitted so removal runs through the executor's
// retire_query_hint policy path (and is retried until the row is gone).
// Hints pg_sage never recorded are left alone.
func (t *Tuner) hintRemovalFindings(ctx context.Context) []analyzer.Finding {
	if t.pool == nil || t.hintPlan == nil || !t.hintPlan.HintTableReady {
		return nil
	}
	rows, err := t.pool.Query(ctx, `/* pg_sage */
SELECT DISTINCT h.query_id,
       (SELECT q.status FROM sage.query_hints q
         WHERE q.queryid = h.query_id ORDER BY q.id DESC LIMIT 1)
FROM hint_plan.hints h
WHERE h.application_name = ''
  AND EXISTS (SELECT 1 FROM sage.query_hints q
               WHERE q.queryid = h.query_id
                 AND q.status IN ('retired', 'broken'))
  AND NOT EXISTS (SELECT 1 FROM sage.query_hints q
                   WHERE q.queryid = h.query_id AND q.status = 'active')
ORDER BY h.query_id`)
	if err != nil {
		t.logFn("WARN", "tuner: reconcile installed hints: %v", err)
		return nil
	}
	defer rows.Close()
	var out []analyzer.Finding
	for rows.Next() {
		var queryID int64
		var status string
		if err := rows.Scan(&queryID, &status); err != nil {
			t.logFn("WARN", "tuner: scan installed hint: %v", err)
			return out
		}
		out = append(out, hintRemovalFinding(queryID, status))
	}
	if err := rows.Err(); err != nil {
		t.logFn("WARN", "tuner: reconcile installed hints rows: %v", err)
	}
	return out
}

func hintRemovalFinding(queryID int64, status string) analyzer.Finding {
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
		},
		Recommendation: "The hint was " + status + " by revalidation but is " +
			"still installed in hint_plan.hints, so PostgreSQL keeps applying it.",
		RecommendedSQL: BuildDeleteSQL(queryID),
		ActionRisk:     "safe",
	}
}
