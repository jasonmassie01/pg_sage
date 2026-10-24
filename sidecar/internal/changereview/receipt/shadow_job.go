package receipt

import (
	"context"
	"fmt"
	"time"
)

// shadowDueSQL lists flagged reviews whose shadow score is settled but not
// recorded. Its WHERE clause is change_review_shadow_due's predicate
// (schema/change_review_receipts_migration.go), so it reads that partial
// index only; keep the two identical.
const shadowDueSQL = `/* pg_sage change_review_receipt_shadow_due v1 */
SELECT id::text, COALESCE(dba_verdict, ''), COALESCE(receipt_verdict, '')
FROM sage.change_review
WHERE shadow_auto_clear AND shadow_score = 'unscored'
  AND (receipt_verdict = 'regressed'
       OR dba_verdict IN ('override', 'concur_with_edits')
       OR (dba_verdict = 'concur'
           AND receipt_verdict IN ('clean', 'clean_low_traffic')))
ORDER BY completed_at LIMIT $1`

type shadowDue struct{ id, dba, receipt string }

// score records the settled shadow scores, once each.
func (j *Job) score(ctx context.Context, db Database, _ time.Time, rep *Report) error {
	rows, err := db.Pool.Query(ctx, shadowDueSQL, MaxShadowPerCycle)
	if err != nil {
		return fmt.Errorf("list shadow scores due: %w", err)
	}
	var due []shadowDue
	for rows.Next() {
		var d shadowDue
		if err := rows.Scan(&d.id, &d.dba, &d.receipt); err != nil {
			rows.Close()
			return fmt.Errorf("scan shadow scores due: %w", err)
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list shadow scores due: %w", err)
	}
	st := db.Service.Store()
	for _, d := range due {
		score := Score(d.dba, d.receipt)
		j.record(rep, d.id, "shadow score", func() error {
			_, err := st.SetShadowScore(ctx, d.id, score)
			return err
		}, &rep.Scored)
	}
	return nil
}
