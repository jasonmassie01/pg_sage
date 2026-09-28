package retention

import (
	"context"
	"strings"
	"testing"
)

// D3 / T5: credited value is pg_sage's evidence of worth, so a credited
// successful action and the verification that earned its credit outlive
// actions_days. Uncredited, zero-credit (rolled back) and failed rows
// still age out as before.
func TestRetentionKeepsCreditedActionLog(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("d3_credit")
	fifteen, zero := 15.0, 0.0
	credited := insertAgedAction(t, ctx, tag, "success", &fifteen)
	uncredited := insertAgedAction(t, ctx, tag, "success", nil)
	failed := insertAgedAction(t, ctx, tag, "failed", &fifteen)
	retracted := insertAgedAction(t, ctx, tag, "rolled_back", &zero)
	decisionID := insertDecision(t, ctx, tag, "400 days")
	keptVerification := insertAgedVerification(t, ctx, decisionID, credited)
	purgedVerification := insertAgedVerification(t, ctx, decisionID, uncredited)

	New(testPool, allDays(30), noopLog).Run(ctx)

	survivors := map[string]int64{
		"action_log":   credited,
		"verification": keptVerification,
		"decision":     decisionID,
	}
	for table, id := range survivors {
		if countWhere(t, ctx, `SELECT count(*) FROM sage.`+table+` WHERE id=$1`, id) != 1 {
			t.Errorf("credited evidence in sage.%s (id %d) was purged", table, id)
		}
	}
	for name, id := range map[string]int64{
		"uncredited": uncredited, "failed": failed, "retracted": retracted,
	} {
		if countWhere(t, ctx, `SELECT count(*) FROM sage.action_log WHERE id=$1`, id) != 0 {
			t.Errorf("%s expired action_log row %d was kept", name, id)
		}
	}
	if countWhere(t, ctx, `SELECT count(*) FROM sage.verification WHERE id=$1`,
		purgedVerification) != 0 {
		t.Error("verification of an uncredited action was kept")
	}
}

func TestKeepActionLogPredicateProtectsCreditedRows(t *testing.T) {
	for _, want := range []string{"outcome", "toil_minutes_saved", "incident_avoided"} {
		if !strings.Contains(keepActionLog, want) {
			t.Errorf("keepActionLog does not mention %q:\n%s", want, keepActionLog)
		}
	}
	if !strings.Contains(keepVerification, "toil_minutes_saved") {
		t.Errorf("keepVerification does not protect credited evidence:\n%s",
			keepVerification)
	}
}

func insertAgedAction(
	t *testing.T, ctx context.Context, tag, outcome string, minutes *float64,
) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.action_log
		(executed_at, action_type, sql_executed, outcome, toil_minutes_saved)
		VALUES (now() - interval '400 days', $1, 'SELECT 1', $2, $3)
		RETURNING id`, tag, outcome, minutes)
}

func insertAgedVerification(
	t *testing.T, ctx context.Context, decisionID, actionID int64,
) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, created_at, completed_at)
		VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 1, now(), now(), 'success',
		        now() - interval '400 days', now() - interval '400 days')
		RETURNING id`, decisionID, actionID)
}
