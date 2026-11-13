package executor

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/verify"
)

// market-08b: only an enforced statement_timeout is judged by its real
// cancels, watched for 24 hours from its first check at 15 minutes.

// No concurrent access tests: classification and windows are pure, and a
// monitor judges one action from one goroutine.

func TestVerificationClass_RoleStatementTimeout(t *testing.T) {
	const enforce = `ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '6000ms'`
	cases := map[string]string{
		enforce: verify.ClassRoleTimeout,
		`ALTER ROLE app IN DATABASE shop SET idle_in_transaction_session_timeout = '60s'`: "",
		`ALTER ROLE "app" IN DATABASE "shop" RESET statement_timeout`:                     "",
		`ALTER ROLE "app" CONNECTION LIMIT 10`:                                            "",
		`ALTER ROLE app SET statement_timeout = '6s'`:                                     "",
	}
	for sql, want := range cases {
		if got := verificationClass(sql); got != want {
			t.Errorf("verificationClass(%q) = %q, want %q", sql, got, want)
		}
	}
}

// Boundary: the class's watch starts at 15 minutes and lasts at least 24
// hours whatever the configured cap; other classes keep theirs.
func TestMonitorWindows_RoleTimeoutWatchesADay(t *testing.T) {
	cfg := RollbackMonitorConfig{WindowMinutes: 60, CapMinutes: 120}
	minW, capW, every := monitorWindows(verify.ClassRoleTimeout, cfg)
	if minW != 15*time.Minute || capW != 24*time.Hour || every != 5*time.Minute {
		t.Fatalf("windows = %s / %s / %s, want 15m / 24h / 5m", minW, capW, every)
	}
	cfg.CapMinutes = 48 * 60
	if _, capW, _ := monitorWindows(verify.ClassRoleTimeout, cfg); capW != 48*time.Hour {
		t.Fatalf("a longer configured cap = %s, want it kept (48h)", capW)
	}
	if minW, capW, _ := monitorWindows(verify.ClassGUC, RollbackMonitorConfig{
		WindowMinutes: 60, CapMinutes: 120}); minW != time.Hour || capW != 2*time.Hour {
		t.Fatalf("guc windows changed: %s / %s", minW, capW)
	}
}

func TestSetRoleTimeoutContract_NamesTheLogCheck(t *testing.T) {
	c := setRoleTimeoutContract()
	const check = "statement-timeout cancels from the server log"
	if !slices.Contains(c.PostChecks, check) || !slices.Contains(c.SuccessCriteria, check) {
		t.Fatalf("contract post-checks %v / success %v lack %q", c.PostChecks,
			c.SuccessCriteria, check)
	}
}

// judgeAt judges the fixture's action at now.
func judgeAt(t *testing.T, f timeoutRevertFixture, now time.Time) judgement {
	t.Helper()
	cfg := revertMonitorConfig(f)
	plan, err := loadMonitorPlan(context.Background(), f.pool, f.actionID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return plan.judge(context.Background(), f.pool, cfg, now)
}

// State transition: past 24 hours within the prediction, counted from the
// server log, the timeout is kept (improved, final).
func TestRoleBudgetRevert_KeptAfterTheWatch(t *testing.T) {
	f := enforceRevertFixture(t, 25*time.Hour, 2)
	recordSource(t, f, budget.CancelSourceLog, 0)
	if j := judgeAt(t, f, time.Now()); j.outcome.Verdict != verify.OutcomeImproved ||
		j.accruing {
		t.Fatalf("after 25 h: %s (%s) accruing %v, want improved and final",
			j.outcome.Verdict, j.outcome.Reason, j.accruing)
	}
}

// recordSource records n of today's cancels from source on the fixture's
// ledger.
func recordSource(t *testing.T, f timeoutRevertFixture, source string, n int64) {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var rows []budget.CancelRow
	if n > 0 {
		rows = []budget.CancelRow{{Budget: revertBudget, Definition: "role=" + budgetRole,
			Day: today, Count: n}}
	}
	if err := f.store.RecordCancels(context.Background(), today, source, rows); err != nil {
		t.Fatal(err)
	}
}

// A late check past the watch (a resumed monitor) never credits cancels
// it cannot prove: without the server log, or past the threshold, the
// action settles uncredited (insufficient, final) and is not reverted.
func TestRoleBudgetRevert_LateCheckCreditsOnlyMeasuredCancels(t *testing.T) {
	f := enforceRevertFixture(t, 30*time.Hour, 0)
	j := judgeAt(t, f, time.Now())
	if j.outcome.Verdict != verify.OutcomeInsufficient || j.accruing ||
		!strings.Contains(j.outcome.Reason, "server log") {
		t.Fatalf("30 h, no log source: %s (%s) accruing %v, want insufficient and final "+
			"naming the log", j.outcome.Verdict, j.outcome.Reason, j.accruing)
	}
	recordSource(t, f, budget.CancelSourceLog, 20)
	j = judgeAt(t, f, time.Now())
	if j.outcome.Verdict != verify.OutcomeInsufficient || j.accruing ||
		!strings.Contains(j.outcome.Reason, "20 cancels") {
		t.Fatalf("30 h, 20 cancels on a zero prediction: %s (%s) accruing %v, want "+
			"insufficient and final naming the count", j.outcome.Verdict, j.outcome.Reason,
			j.accruing)
	}
}

// Only cancels after the action count: the ledger's count at apply
// (before_state.cancel_watch.actual_at_apply) is subtracted.
func TestRoleBudgetRevert_CancelsBeforeApplyDoNotCount(t *testing.T) {
	f := enforceRevertFixture(t, 3*time.Hour, 2) // threshold 2 x 6
	recordSource(t, f, budget.CancelSourceLog, 20)
	setActualAtApply := func(n int64) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(), `UPDATE sage.action_log
			SET before_state = jsonb_set(before_state, '{cancel_watch,actual_at_apply}',
			    to_jsonb($2::bigint)) WHERE id = $1`, f.actionID, n); err != nil {
			t.Fatal(err)
		}
	}
	setActualAtApply(10)
	if j := judgeAt(t, f, time.Now()); j.outcome.Verdict != verify.OutcomeInsufficient ||
		!strings.Contains(j.outcome.Reason, "10 cancels") {
		t.Fatalf("20 cancels, 10 at apply: %s (%s), want 10 counted and kept",
			j.outcome.Verdict, j.outcome.Reason)
	}
	setActualAtApply(7)
	if j := judgeAt(t, f, time.Now()); j.outcome.Verdict != verify.OutcomeRegressed ||
		!strings.Contains(j.outcome.Reason, "13 cancels") {
		t.Fatalf("20 cancels, 7 at apply: %s (%s), want 13 counted and regressed",
			j.outcome.Verdict, j.outcome.Reason)
	}
}

// A lost log source keeps watching and says why; a zero prediction
// reverts only past the floor (11 cancels).
func TestRoleBudgetRevert_LostSourceAndZeroPrediction(t *testing.T) {
	g := enforceRevertFixture(t, time.Hour, 0)
	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := g.store.RecordCancels(ctx, today, budget.CancelSourceNone, nil); err != nil {
		t.Fatal(err)
	}
	j := judgeAt(t, g, time.Now())
	if j.outcome.Verdict != verify.OutcomeInsufficient || !j.accruing ||
		!strings.Contains(j.outcome.Reason, "not read now") {
		t.Fatalf("no log source: %s (%s), want insufficient naming the log",
			j.outcome.Verdict, j.outcome.Reason)
	}
	rows := []budget.CancelRow{{Budget: revertBudget, Definition: "role=" + budgetRole,
		Day: today, Count: 10}}
	if err := g.store.RecordCancels(ctx, today, budget.CancelSourceLog, rows); err != nil {
		t.Fatal(err)
	}
	if j := judgeAt(t, g, time.Now()); j.outcome.Verdict != verify.OutcomeInsufficient {
		t.Fatalf("10 cancels on a zero prediction: %s (%s), want kept",
			j.outcome.Verdict, j.outcome.Reason)
	}
	rows[0].Count = 1
	if err := g.store.RecordCancels(ctx, today, budget.CancelSourceLog, rows); err != nil {
		t.Fatal(err)
	}
	if j := judgeAt(t, g, time.Now()); j.outcome.Verdict != verify.OutcomeRegressed {
		t.Fatalf("11 cancels on a zero prediction: %s (%s), want regressed",
			j.outcome.Verdict, j.outcome.Reason)
	}
}

// predictAction records the finding's cancel watch with the ledger's
// count at apply, so cancels before the action never count; a malformed
// watch records none.
func TestPredictAction_RecordsTheCancelWatch(t *testing.T) {
	f := enforceRevertFixture(t, time.Hour, 2)
	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if err := f.store.RecordCancels(ctx, today, budget.CancelSourceLog, []budget.CancelRow{{
		Budget: revertBudget, Definition: "role=" + budgetRole, Day: today,
		Count: 4}}); err != nil {
		t.Fatal(err)
	}
	detail := map[string]any{"cancel_watch": map[string]any{"budget": revertBudget,
		"ledger_database": "revert-fleet", "role": budgetRole, "predicted_per_hour": 2.0,
		"source": budget.CancelSourceLog}, "predicted_effect": map[string]any{
		"method": "rule", "metric": verify.MetricStatementTimeoutCancels,
		"expected_change_pct": 0.0, "baseline": 2.0}}
	before := map[string]any{}
	p := f.e.predictAction(ctx, f.sql, detail, before)
	w, ok := before["cancel_watch"].(cancelWatch)
	if !ok || w.ActualAtApply != 4 || w.PredictedPerHour != 2 || w.Budget != revertBudget {
		t.Fatalf("cancel_watch = %#v, want budget %s with 4 cancels at apply",
			before["cancel_watch"], revertBudget)
	}
	if !p.Predicts() || p.Class != verify.ClassRoleTimeout || p.Method != verify.MethodRule {
		t.Fatalf("prediction = %+v, want the budget's rule prediction", p)
	}
	bad := map[string]any{}
	f.e.predictAction(ctx, f.sql, map[string]any{"cancel_watch": "nonsense"}, bad)
	if _, ok := bad["cancel_watch"]; ok {
		t.Fatalf("a malformed watch was recorded: %#v", bad["cancel_watch"])
	}
}
