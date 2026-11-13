package analyzer

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-13: subscriber health. subscription_errors fires on an apply or
// sync error counter that rose since the analyzer last looked (warning), or
// on a disabled subscription with errors (critical: disable_on_error switched
// it off). subscription_lag fires when latest_end_time is older than
// analyzer.subscription_lag_minutes, or when an enabled subscription had no
// apply worker in two consecutive snapshots. Both are advisory: ALTER
// SUBSCRIPTION is only ever detail.manual_sql.
//
// No concurrent-access tests: the rules run on the analyzer's goroutine and
// their counter baseline lives in that analyzer's RuleExtras.

var subT0 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func i64(v int64) *int64 { return &v }
func i32(v int32) *int32 { return &v }

// liveSub is an enabled subscription with a running apply worker that
// reported to its publisher a second ago, on a PG15+ server.
func liveSub(name string, applyErrors int64) collector.SubscriptionInfo {
	return collector.SubscriptionInfo{
		Name: name, Enabled: true, WorkerPID: i32(4242),
		LatestEndTime:   timePtr(subT0.Add(-time.Second)),
		ApplyErrorCount: i64(applyErrors), SyncErrorCount: i64(0),
	}
}

func subSnap(at time.Time, subs ...collector.SubscriptionInfo) *collector.Snapshot {
	if subs == nil {
		subs = []collector.SubscriptionInfo{}
	}
	return &collector.Snapshot{CollectedAt: at, Subscriptions: subs}
}

func subExtras(at *time.Time) *RuleExtras {
	return &RuleExtras{Now: func() time.Time { return *at }}
}

func TestSubscriptionErrors_DeltaAboveZeroWarnsThenClears(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	cfg := phase2Config()
	got := ruleSubscriptionErrors(subSnap(now, liveSub("orders", 0)), nil, cfg, ex)
	if len(got) != 0 {
		t.Fatalf("baseline cycle: %+v, want none", got)
	}
	now = subT0.Add(10 * time.Minute)
	got = ruleSubscriptionErrors(subSnap(now, liveSub("orders", 2)), nil, cfg, ex)
	if len(got) != 1 {
		t.Fatalf("errors rose 0->2: %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Category != "subscription_errors" || f.Severity != "warning" ||
		f.ObjectIdentifier != "subscription:orders" || f.ObjectType != "subscription" {
		t.Fatalf("finding = %+v, want a subscription_errors warning on subscription:orders", f)
	}
	if f.Detail["error_count_delta"] != int64(2) || f.Detail["apply_error_count"] != int64(2) {
		t.Errorf("detail = %v, want error_count_delta 2 and apply_error_count 2", f.Detail)
	}
	sql, _ := f.Detail["manual_sql"].(string)
	if f.RecommendedSQL != "" || f.ActionRisk != "high_risk" ||
		!strings.Contains(sql, `ALTER SUBSCRIPTION "orders" SKIP`) {
		t.Errorf("sql=%q risk=%q manual_sql=%q, want advisory high_risk with a SKIP template",
			f.RecommendedSQL, f.ActionRisk, sql)
	}
	if f.Detail[DetailApprovalRequired] == nil {
		t.Error("detail.approval_required missing")
	}
	now = subT0.Add(20 * time.Minute)
	got = ruleSubscriptionErrors(subSnap(now, liveSub("orders", 2)), nil, cfg, ex)
	if len(got) != 0 {
		t.Fatalf("no new errors: %+v, want none", got)
	}
}

func TestSubscriptionErrors_SyncErrorsCount(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	s := liveSub("orders", 0)
	ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex)
	s.SyncErrorCount = i64(1)
	got := ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex)
	if len(got) != 1 || got[0].Detail["sync_error_count"] != int64(1) {
		t.Fatalf("sync error rose: %+v, want one warning carrying sync_error_count", got)
	}
}

// A stats reset (pg_stat_reset_subscription_stats) lowers the counter: the
// errors since the reset count, never a negative delta.
func TestSubscriptionErrors_CounterResetCountsFromZero(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	ruleSubscriptionErrors(subSnap(now, liveSub("orders", 5)), nil, phase2Config(), ex)
	got := ruleSubscriptionErrors(subSnap(now, liveSub("orders", 2)), nil, phase2Config(), ex)
	if len(got) != 1 || got[0].Detail["error_count_delta"] != int64(2) {
		t.Fatalf("after reset 5->2: %+v, want one warning with delta 2", got)
	}
	got = ruleSubscriptionErrors(subSnap(now, liveSub("orders", 0)), nil, phase2Config(), ex)
	if len(got) != 0 {
		t.Fatalf("reset to 0: %+v, want none", got)
	}
}

func TestSubscriptionErrors_DisabledWithErrorsIsCritical(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	s := liveSub(`we"ird`, 3)
	s.Enabled, s.WorkerPID = false, nil
	got := ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex)
	if len(got) != 1 || got[0].Severity != "critical" {
		t.Fatalf("disabled with errors on first sight: %+v, want one critical", got)
	}
	sql, _ := got[0].Detail["manual_sql"].(string)
	if !strings.Contains(sql, `ALTER SUBSCRIPTION "we""ird" ENABLE`) ||
		got[0].RecommendedSQL != "" {
		t.Fatalf("manual_sql = %q sql=%q, want a quoted ENABLE and no executable SQL",
			sql, got[0].RecommendedSQL)
	}
	if got[0].Detail["enabled"] != false {
		t.Errorf("detail.enabled = %v, want false", got[0].Detail["enabled"])
	}
}

func TestSubscriptionErrors_DisabledWithoutErrorsIsSilent(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	s := liveSub("paused", 0)
	s.Enabled, s.WorkerPID = false, nil
	for i := 0; i < 2; i++ {
		if got := ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex); len(got) != 0 {
			t.Fatalf("cycle %d: an operator-disabled subscription raised %+v", i, got)
		}
	}
}

// Boundary (version): PG14 has no pg_stat_subscription_stats, so the
// counters are nil and nothing can be said about errors.
func TestSubscriptionErrors_PG14CountersNilNoFinding(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	s := liveSub("orders", 0)
	s.ApplyErrorCount, s.SyncErrorCount = nil, nil
	s.Enabled = false
	for i := 0; i < 2; i++ {
		if got := ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex); len(got) != 0 {
			t.Fatalf("cycle %d: PG14 subscription raised %+v", i, got)
		}
	}
}

// PG18 conflict counters ride along in the detail of an error finding.
func TestSubscriptionErrors_ConflictCountersInDetail(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	s := liveSub("orders", 0)
	ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex)
	s.ApplyErrorCount = i64(1)
	s.Conflicts = map[string]int64{"confl_insert_exists": 1, "confl_update_missing": 0}
	got := ruleSubscriptionErrors(subSnap(now, s), nil, phase2Config(), ex)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	c, ok := got[0].Detail["conflicts"].(map[string]int64)
	if !ok || c["confl_insert_exists"] != 1 {
		t.Fatalf("detail.conflicts = %#v, want the PG18 counters", got[0].Detail["conflicts"])
	}
}

// An unread cycle (nil subscriptions) keeps the baseline: the errors raised
// across the gap still count when the category is read again.
func TestSubscriptionErrors_UnreadCycleKeepsBaseline(t *testing.T) {
	now := subT0
	ex := subExtras(&now)
	ruleSubscriptionErrors(subSnap(now, liveSub("orders", 1)), nil, phase2Config(), ex)
	gap := &collector.Snapshot{CollectedAt: now,
		Unavailable: map[string]string{"subscriptions": "permission denied"}}
	if got := ruleSubscriptionErrors(gap, nil, phase2Config(), ex); got != nil {
		t.Fatalf("unread cycle: %+v, want nil", got)
	}
	got := ruleSubscriptionErrors(subSnap(now, liveSub("orders", 4)), nil, phase2Config(), ex)
	if len(got) != 1 || got[0].Detail["error_count_delta"] != int64(3) {
		t.Fatalf("after the gap: %+v, want one warning with delta 3", got)
	}
}

func lagFindings(cur, prev *collector.Snapshot, cfg *config.Config) []Finding {
	return ruleSubscriptionLag(cur, prev, cfg, &RuleExtras{})
}

// Boundary: a lag of subscription_lag_minutes - 1 gives no finding, the
// threshold itself a warning.
func TestSubscriptionLag_Boundary(t *testing.T) {
	for _, tc := range []struct {
		setting int
		lag     time.Duration
		want    int
	}{
		{0, 4 * time.Minute, 0}, {0, 5 * time.Minute, 1},
		{10, 9 * time.Minute, 0}, {10, 10 * time.Minute, 1}, {10, 2 * time.Hour, 1},
	} {
		cfg := phase2Config()
		cfg.Analyzer.SubscriptionLagMinutes = tc.setting
		s := liveSub("orders", 0)
		s.LatestEndTime = timePtr(subT0.Add(-tc.lag))
		got := lagFindings(subSnap(subT0, s), subSnap(subT0.Add(-time.Minute), s), cfg)
		if len(got) != tc.want {
			t.Fatalf("setting %d lag %s: %d findings, want %d", tc.setting, tc.lag, len(got),
				tc.want)
		}
		if tc.want == 1 && (got[0].Category != "subscription_lag" ||
			got[0].ObjectIdentifier != "subscription:orders" || got[0].RecommendedSQL != "") {
			t.Fatalf("finding = %+v, want an advisory subscription_lag", got[0])
		}
		if tc.want == 1 && got[0].Detail["lag_seconds"] != tc.lag.Seconds() {
			t.Errorf("detail.lag_seconds = %v, want %v", got[0].Detail["lag_seconds"],
				tc.lag.Seconds())
		}
	}
}

// State transition: no worker once is a restart (the launcher restarts a
// failed apply worker within seconds); no worker in two consecutive
// snapshots is a finding.
func TestSubscriptionLag_NoWorkerTwoSnapshotsFires(t *testing.T) {
	cfg := phase2Config()
	gone := liveSub("orders", 0)
	gone.WorkerPID, gone.LatestEndTime = nil, nil
	running := liveSub("orders", 0)
	prevAt := subT0.Add(-time.Minute)
	if got := lagFindings(subSnap(subT0, gone), subSnap(prevAt, running), cfg); len(got) != 0 {
		t.Fatalf("worker gone once: %+v, want none", got)
	}
	got := lagFindings(subSnap(subT0, gone), subSnap(prevAt, gone), cfg)
	if len(got) != 1 || got[0].Detail["worker_running"] != false {
		t.Fatalf("worker gone twice: %+v, want one finding with worker_running false", got)
	}
}

func TestSubscriptionLag_DisabledSubscriptionSkipped(t *testing.T) {
	s := liveSub("paused", 0)
	s.Enabled, s.WorkerPID = false, nil
	s.LatestEndTime = timePtr(subT0.Add(-time.Hour))
	if got := lagFindings(subSnap(subT0, s), subSnap(subT0, s), phase2Config()); len(got) != 0 {
		t.Fatalf("disabled subscription: %+v, want none (subscription_errors owns it)", got)
	}
}

// subCycle runs one cycle's snapshot rules and reports whether category may
// resolve, the identities kept open, and how many findings it emitted.
func subCycle(a *Analyzer, cur, prev *collector.Snapshot, category string) (bool,
	[]string, int) {
	a.eval = newCycleEval()
	out := a.runSnapshotRules(cur, prev, false)
	n := 0
	for _, f := range out {
		if f.Category == category {
			n++
		}
	}
	var kept []string
	for id := range a.eval.kept(category) {
		kept = append(kept, id)
	}
	sort.Strings(kept)
	return a.eval.resolvable(out)[category], kept, n
}

// Nil/empty: no subscriptions is an empty, available category, so open
// findings resolve; an unread category resolves nothing.
func TestSubscriptionRules_EmptyIsEvaluatedUnreadIsNot(t *testing.T) {
	clk := &slotTestClock{now: subT0}
	a := slotHoldAnalyzer(clk)
	empty := subSnap(subT0)
	for _, cat := range []string{"subscription_errors", "subscription_lag"} {
		if ok, kept, n := subCycle(a, empty, subSnap(subT0), cat); !ok || len(kept) != 0 || n != 0 {
			t.Errorf("%s empty: resolvable=%v kept=%v n=%d, want resolvable", cat, ok, kept, n)
		}
	}
	unread := &collector.Snapshot{CollectedAt: subT0,
		Unavailable: map[string]string{"subscriptions": "permission denied"}}
	for _, cat := range []string{"subscription_errors", "subscription_lag"} {
		if ok, _, _ := subCycle(a, unread, subSnap(subT0), cat); ok {
			t.Errorf("%s unread: resolvable, want left untouched", cat)
		}
	}
}

// After a restart the error baseline is empty: an enabled subscription seen
// for the first time is undecided, so its open warning stays open.
func TestSubscriptionErrors_FirstSightHoldsOpenFinding(t *testing.T) {
	clk := &slotTestClock{now: subT0}
	a := slotHoldAnalyzer(clk)
	snap := subSnap(subT0, liveSub("orders", 7))
	ok, kept, n := subCycle(a, snap, snap, "subscription_errors")
	if !ok || n != 0 || len(kept) != 1 || kept[0] != "subscription:orders" {
		t.Fatalf("first sight: resolvable=%v kept=%v n=%d, want subscription:orders kept", ok,
			kept, n)
	}
	ok, kept, n = subCycle(a, snap, snap, "subscription_errors")
	if !ok || n != 0 || len(kept) != 0 {
		t.Fatalf("second sight, no new errors: resolvable=%v kept=%v n=%d, want it to resolve",
			ok, kept, n)
	}
}

// A worker missing now but present (or unknown) before is undecided: an
// open no-worker finding is kept, not resolved and reopened.
func TestSubscriptionLag_UndecidedWorkerHoldsOpenFinding(t *testing.T) {
	clk := &slotTestClock{now: subT0}
	a := slotHoldAnalyzer(clk)
	gone := liveSub("orders", 0)
	gone.WorkerPID, gone.LatestEndTime = nil, nil
	ok, kept, n := subCycle(a, subSnap(subT0, gone), nil, "subscription_lag")
	if n != 0 || (ok && (len(kept) != 1 || kept[0] != "subscription:orders")) {
		t.Fatalf("no previous snapshot: resolvable=%v kept=%v n=%d, want held", ok, kept, n)
	}
}
