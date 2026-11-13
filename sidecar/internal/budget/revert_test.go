package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// market-08b: an enforced statement_timeout reverts when its real cancels
// exceed twice the shadow prediction; the monitor records the cancels and
// keeps a reverted budget in warn mode for a fresh 7 days.

// No concurrent access tests here: the rule and the prediction are pure
// functions; cancels_test covers the counter's concurrency.

func check(actual int64, perHour float64, elapsed time.Duration) bool {
	revert, _ := RevertCheck{Actual: actual, PredictedPerHour: perHour,
		Elapsed: elapsed}.Decide()
	return revert
}

// Happy path: prediction 2/h, 30 cancels in 3 h (more than 2 x 6)
// reverts; 11 cancels in 3 h do not.
func TestRevertCheck_HappyPath(t *testing.T) {
	revert, reason := RevertCheck{Actual: 30, PredictedPerHour: 2,
		Elapsed: 3 * time.Hour}.Decide()
	if !revert {
		t.Fatal("30 cancels in 3 h against 2/h did not revert")
	}
	for _, want := range []string{"30 cancels in 3.0 h", "prediction 6.0", "2.0/h"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	if check(11, 2, 3*time.Hour) {
		t.Fatal("11 cancels in 3 h against 2/h reverted")
	}
}

// Boundary: exactly 2x the prediction does not revert, one more does; 14
// minutes never revert; a zero prediction needs 11 cancels (2 x floor 5,
// plus one); past the 24 h watch nothing reverts.
func TestRevertCheck_Boundaries(t *testing.T) {
	cases := []struct {
		name    string
		actual  int64
		perHour float64
		elapsed time.Duration
		want    bool
	}{
		{"exactly 2x", 12, 2, 3 * time.Hour, false},
		{"2x plus one", 13, 2, 3 * time.Hour, true},
		{"14 minutes", 1000, 2, 14 * time.Minute, false},
		{"15 minutes", 1000, 2, 15 * time.Minute, true},
		{"zero prediction at the floor", 10, 0, time.Hour, false},
		{"zero prediction past the floor", 11, 0, time.Hour, true},
		{"24 h watch ended", 1000, 2, 24*time.Hour + time.Minute, false},
		{"negative prediction is zero", 11, -4, time.Hour, true},
	}
	for _, tc := range cases {
		if got := check(tc.actual, tc.perHour, tc.elapsed); got != tc.want {
			t.Errorf("%s: revert = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// day is one ledger row on t0's day minus back days.
func day(back int, wouldCancel int, actual int64) ShadowDay {
	d := utcDay(t0.Add(-time.Duration(back) * 24 * time.Hour))
	return ShadowDay{Day: d, FirstSeen: d, WouldCancel: wouldCancel, ActualCancels: actual}
}

// The prediction is the mean daily would-cancels plus actual cancels over
// the 7 full days before today, per hour; today's partial row and older
// rows are left out.
func TestPredictedPerHour(t *testing.T) {
	history := []ShadowDay{day(9, 1000, 1000)}
	for back := 7; back >= 1; back-- {
		history = append(history, day(back, 36, 12))
	}
	history = append(history, day(0, 5000, 5000))
	perHour, days := PredictedPerHour(history, t0)
	if days != 7 || perHour != 2 {
		t.Fatalf("prediction = %.3f/h over %d days, want 2/h over 7", perHour, days)
	}
	if perHour, days := PredictedPerHour(nil, t0); perHour != 0 || days != 0 {
		t.Fatalf("empty history = %.3f over %d days", perHour, days)
	}
}

// State transition: a revert restarts the promotion window; 7 days after
// it the budget may promote again.
func TestCanPromoteSince_RevertRestartsTheWindow(t *testing.T) {
	days := ledgerDays(8, -1)
	if ok, why := CanPromoteSince(days, t0, time.Time{}); !ok {
		t.Fatalf("no revert: refused (%s)", why)
	}
	reverted := t0.Add(-2 * 24 * time.Hour)
	ok, why := CanPromoteSince(days, t0, reverted)
	if ok || !strings.Contains(why, "reverted") {
		t.Fatalf("2 days after a revert: ok=%v (%s), want a refusal naming the revert", ok, why)
	}
	later := t0.Add(5*24*time.Hour + time.Hour)
	more := append(append([]ShadowDay{}, days...), ledgerAfter(6)...)
	if ok, why := CanPromoteSince(more, later, reverted); !ok {
		t.Fatalf("7 days after the revert: refused (%s)", why)
	}
}

// ledgerAfter is one row per UTC day for n days after t0's day.
func ledgerAfter(n int) []ShadowDay {
	var out []ShadowDay
	for i := 1; i <= n; i++ {
		at := t0.Add(time.Duration(i) * 24 * time.Hour)
		out = append(out, ShadowDay{Day: utcDay(at), FirstSeen: utcDay(at)})
	}
	return out
}

// fakeCancelLedger is a fakeLedger that also records cancels and reports
// reverts (the Store's CancelLedger).
type fakeCancelLedger struct {
	fakeLedger
	source     string
	cancels    []CancelRow
	reverted   time.Time
	revertSQL  string
	cancelErr  error
	revertErr  error
	cancelDays []time.Time
}

func (l *fakeCancelLedger) RecordCancels(_ context.Context, today time.Time, source string,
	rows []CancelRow) error {
	l.source = source
	l.cancelDays = append(l.cancelDays, today)
	if l.cancelErr != nil {
		return l.cancelErr
	}
	l.cancels = append(l.cancels, rows...)
	return nil
}

func (l *fakeCancelLedger) LastRevert(_ context.Context, sql string) (time.Time, error) {
	l.revertSQL = sql
	return l.reverted, l.revertErr
}

func enforceMonitor(l *fakeCancelLedger, counter *CancelCounter) *Monitor {
	c := fakeCatalog{self: "sage", facts: map[string]RoleFact{"app": {
		Settings: map[string]string{"statement_timeout": "30s"}}}}
	m := NewMonitor(enforceCfg(), fakeSource{snapWith()}, l, c, "shop-fleet",
		func(string, string, ...any) {})
	m.now = func() time.Time { return t0 }
	if counter != nil {
		m.WithCancelCounter(counter)
	}
	return m
}

func timeoutFinding(t *testing.T, fs []analyzer.Finding) analyzer.Finding {
	t.Helper()
	for _, f := range findingsOf(fs, CategoryEnforce) {
		if f.Detail["setting"] == SettingStatementTimeout {
			return f
		}
	}
	t.Fatalf("no statement_timeout enforce finding in %+v", fs)
	return analyzer.Finding{}
}

// Happy path: with a log source the monitor writes the role's cancels to
// each budget matching the role, in warn mode too, and the enforce
// finding carries its prediction and runs without approval.
func TestMonitor_RecordsCancelsAndPredicts(t *testing.T) {
	src := &fakeEntries{}
	counter := NewCancelCounter("shop", src, alwaysActive)
	src.push(timeoutEntry("app", "shop", t0), timeoutEntry("app", "shop", t0),
		timeoutEntry("other", "shop", t0))
	counter.Poll(t0)
	l := &fakeCancelLedger{fakeLedger: fakeLedger{history: sevenDays()}}
	fs, err := enforceMonitor(l, counter).Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.source != CancelSourceLog {
		t.Fatalf("cancel source = %q, want log", l.source)
	}
	var app int64
	for _, r := range l.cancels {
		if r.Budget != "app-6s" || r.Definition == "" {
			t.Fatalf("cancel row for %+v: only budget app-6s matches role app", r)
		}
		app += r.Count
	}
	if app != 2 {
		t.Fatalf("app-6s cancels = %d (rows %+v), want 2", app, l.cancels)
	}
	f := timeoutFinding(t, fs)
	if _, ok := f.Detail[analyzer.DetailApprovalRequired]; ok {
		t.Fatalf("a budget with a log source needs approval: %+v", f.Detail)
	}
	watch, ok := f.Detail["cancel_watch"].(map[string]any)
	if !ok || watch["budget"] != "app-6s" || watch["ledger_database"] != "shop-fleet" ||
		watch["role"] != "app" || watch["source"] != CancelSourceLog {
		t.Fatalf("cancel_watch = %+v", f.Detail["cancel_watch"])
	}
	if _, ok := watch["predicted_per_hour"].(float64); !ok {
		t.Fatalf("predicted_per_hour = %#v, want a float", watch["predicted_per_hour"])
	}
	if p, ok := f.Detail["predicted_effect"].(map[string]any); !ok || p["method"] != "rule" {
		t.Fatalf("predicted_effect = %+v", f.Detail["predicted_effect"])
	}
}

// A day the log subscription dropped entries is recorded as a lower
// bound on the role budget's row.
func TestMonitor_RecordsTheLowerBound(t *testing.T) {
	src := &fakeEntries{evicted: 3}
	counter := NewCancelCounter("shop", src, alwaysActive)
	counter.Poll(t0)
	l := &fakeCancelLedger{fakeLedger: fakeLedger{history: sevenDays()}}
	if _, err := enforceMonitor(l, counter).Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range l.cancels {
		if r.Budget == "app-6s" && r.Day.Equal(utcDay(t0)) {
			found = true
			if !r.LowerBound || r.Count != 0 {
				t.Fatalf("row %+v, want a lower bound with no cancels", r)
			}
		}
	}
	if !found {
		t.Fatalf("no row for app-6s on %s in %+v", utcDay(t0), l.cancels)
	}
}

// Nil/empty: no subscription records source none and downgrades the
// statement_timeout enforce finding to approval with the card text.
func TestMonitor_NoLogSourceNeedsApproval(t *testing.T) {
	l := &fakeCancelLedger{fakeLedger: fakeLedger{history: sevenDays()}}
	fs, err := enforceMonitor(l, nil).Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.source != CancelSourceNone || len(l.cancels) != 0 {
		t.Fatalf("source %q rows %+v, want none and no counts", l.source, l.cancels)
	}
	f := timeoutFinding(t, fs)
	if got := f.Detail[analyzer.DetailApprovalRequired]; got != NoLogApprovalReason {
		t.Fatalf("approval_required = %#v, want %q", got, NoLogApprovalReason)
	}
	if !strings.Contains(f.Recommendation, "auto-revert unavailable: no statement log") {
		t.Fatalf("recommendation lacks the card text: %q", f.Recommendation)
	}
}

// Error propagation: a failed cancel write keeps the counts for the next
// cycle and fails the cycle with context; a failed revert read fails it.
func TestMonitor_CancelLedgerErrors(t *testing.T) {
	src := &fakeEntries{}
	counter := NewCancelCounter("shop", src, alwaysActive)
	src.push(timeoutEntry("app", "shop", t0))
	counter.Poll(t0)
	l := &fakeCancelLedger{fakeLedger: fakeLedger{history: sevenDays()},
		cancelErr: errors.New("connection refused")}
	_, err := enforceMonitor(l, counter).Detect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "statement-timeout cancels") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the cancel write named with its cause", err)
	}
	if got := countOf(counter.Take(), "app", utcDay(t0)); got != 1 {
		t.Fatalf("a failed write lost the counts: %d left, want 1", got)
	}
	l2 := &fakeCancelLedger{fakeLedger: fakeLedger{history: sevenDays()},
		revertErr: errors.New("permission denied for table action_log")}
	_, err = enforceMonitor(l2, counter).Detect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("err = %v, want the revert read named", err)
	}
}

// State transitions: warn -> enforce -> reverted -> warn. A revert two
// days ago keeps the budget in warn mode (no action) though its ledger
// continues; the revert is looked up by the enforce statement.
func TestMonitor_RevertedBudgetStaysInWarn(t *testing.T) {
	l := &fakeCancelLedger{fakeLedger: fakeLedger{history: sevenDays()},
		reverted: t0.Add(-48 * time.Hour)}
	fs, err := enforceMonitor(l, nil).Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findingsOf(fs, CategoryEnforce) {
		if f.Detail["setting"] == SettingStatementTimeout {
			t.Fatalf("a reverted budget was re-enforced: %+v", f)
		}
	}
	want := `ALTER ROLE "app" IN DATABASE "shop" SET statement_timeout = '6000ms'`
	if l.revertSQL != want {
		t.Fatalf("revert looked up by %q, want %q", l.revertSQL, want)
	}
}
