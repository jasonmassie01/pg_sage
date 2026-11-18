package earned

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ux-07a: the approvals a person makes are evidence. countApprovals
// counts, over the window, the person-approved (L2) executions whose
// verdict is improved or neutral, since the last rollback, regression or
// rejection; the count can create a proposal, never a grant.

// No concurrent access tests here: countApprovals, AssessApprovalHistory,
// AllowAlways and ApprovalAsk are pure functions of their arguments
// (the concurrent grant is covered by approval_history_db_test.go).

var histNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func approvedMark(id int64, verdict string, at time.Time) approvalMark {
	result := ResultVerifiedRecovery
	if verdict == "neutral" {
		result = ResultUnverified
	}
	return approvalMark{actionLogID: id, level: L2, source: SourceExecutor, verdict: verdict,
		result: result, at: at}
}

func approvals(n int, start time.Time) []approvalMark {
	out := make([]approvalMark, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, approvedMark(int64(i+1), "improved", start.Add(time.Duration(i)*
			time.Hour)))
	}
	return out
}

func since() time.Time { return histNow.Add(-ApprovalHistoryWindow) }

func TestCountApprovals_NineDoNotQualifyTenDo(t *testing.T) {
	start := histNow.Add(-5 * 24 * time.Hour)
	nine := countApprovals(approvals(9, start), since(), 10)
	if nine.Approvals != 9 || nine.Qualifies || nine.Required != 10 {
		t.Fatalf("9 approvals = %+v, want 9 that do not qualify", nine)
	}
	ten := countApprovals(approvals(10, start), since(), 10)
	if ten.Approvals != 10 || !ten.Qualifies || ten.Improved != 10 {
		t.Fatalf("10 approvals = %+v, want 10 that qualify", ten)
	}
}

func TestCountApprovals_NeutralCountsAndMixesWithImproved(t *testing.T) {
	marks := approvals(6, histNow.Add(-48*time.Hour))
	for i := 0; i < 4; i++ {
		marks = append(marks, approvedMark(int64(100+i), "neutral",
			histNow.Add(-time.Duration(i+1)*time.Minute)))
	}
	h := countApprovals(marks, since(), 10)
	if h.Approvals != 10 || h.Improved != 6 || h.Neutral != 4 || !h.Qualifies {
		t.Fatalf("history = %+v, want 6 improved + 4 neutral = 10", h)
	}
}

func TestCountApprovals_OneRollbackInsideTheWindowResetsTheCount(t *testing.T) {
	marks := approvals(12, histNow.Add(-10*24*time.Hour))
	rollbackAt := histNow.Add(-3 * 24 * time.Hour)
	marks = append(marks, approvalMark{actionLogID: 5, level: L2, source: SourceRollback,
		verdict: CauseRolledBack, result: ResultRejected, at: rollbackAt})
	marks = append(marks, approvals(2, rollbackAt.Add(time.Hour))...)
	marks[len(marks)-1].actionLogID, marks[len(marks)-2].actionLogID = 501, 502
	h := countApprovals(marks, since(), 10)
	if h.Approvals != 2 || h.Qualifies || h.RolledBack != 1 || h.LastResetAt == nil ||
		!h.LastResetAt.Equal(rollbackAt) || h.LastReset != CauseRolledBack {
		t.Fatalf("history = %+v, want the count reset by the rollback to 2", h)
	}
}

func TestCountApprovals_RegressionAndRejectionReset(t *testing.T) {
	for _, reset := range []approvalMark{
		{actionLogID: 900, level: L2, source: SourceExecutor, verdict: "regressed",
			result: ResultHarmful},
		{level: L2, source: SourceOperator, verdict: CauseRejected, result: ResultRejected},
	} {
		reset.at = histNow.Add(-time.Hour)
		marks := append(approvals(11, histNow.Add(-9*24*time.Hour)), reset)
		h := countApprovals(marks, since(), 10)
		if h.Approvals != 0 || h.Qualifies || h.LastResetAt == nil {
			t.Fatalf("after %s: %+v, want the count reset to 0", reset.verdict, h)
		}
	}
}

func TestCountApprovals_ARollbackOutsideTheWindowDoesNotReset(t *testing.T) {
	old := approvalMark{actionLogID: 1, level: L2, source: SourceRollback,
		verdict: CauseRolledBack, result: ResultRejected, at: since().Add(-time.Hour)}
	marks := append([]approvalMark{old}, approvals(10, histNow.Add(-24*time.Hour))...)
	h := countApprovals(marks, since(), 10)
	if h.Approvals != 10 || !h.Qualifies || h.RolledBack != 0 {
		t.Fatalf("history = %+v, want the old rollback ignored", h)
	}
}

func TestCountApprovals_OnlyPersonApprovedExecutionsCount(t *testing.T) {
	marks := approvals(8, histNow.Add(-24*time.Hour))
	l3 := approvedMark(50, "improved", histNow.Add(-time.Minute))
	l3.level = L3 // ran unattended: not a person's approval
	l1 := approvedMark(51, "improved", histNow.Add(-time.Minute))
	l1.level = L1
	shadow := approvedMark(52, "improved", histNow.Add(-time.Minute))
	shadow.source = SourceOperator
	insufficient := approvedMark(53, "insufficient_evidence", histNow.Add(-time.Minute))
	old := approvedMark(54, "improved", since().Add(-time.Second))
	h := countApprovals(append(marks, l3, l1, shadow, insufficient, old), since(), 10)
	if h.Approvals != 8 || h.Qualifies {
		t.Fatalf("history = %+v, want only the 8 L2 executor verdicts", h)
	}
}

func TestCountApprovals_ARolledBackActionsOwnVerdictIsNoCredit(t *testing.T) {
	// Actions 1..10 decided at -24h..-15h. Action 10 was rolled back at
	// -15h30m, before its verdict was recorded at -15h: the rollback resets
	// the count, and action 10's later verdict is no credit either.
	marks := approvals(10, histNow.Add(-24*time.Hour))
	marks = append(marks, approvalMark{actionLogID: 10, level: L2, source: SourceRollback,
		verdict: CauseRolledBack, result: ResultRejected,
		at: histNow.Add(-15*time.Hour - 30*time.Minute)})
	h := countApprovals(marks, since(), 10)
	if h.Approvals != 0 || h.Qualifies || h.RolledBack != 1 {
		t.Fatalf("history = %+v, want 0 (action 10 rolled back)", h)
	}
}

func TestCountApprovals_EmptyAndZeroRequired(t *testing.T) {
	if h := countApprovals(nil, since(), 10); h.Approvals != 0 || h.Qualifies ||
		h.Required != 10 || h.LastResetAt != nil {
		t.Fatalf("nil marks = %+v", h)
	}
	if h := countApprovals([]approvalMark{}, since(), 10); h.Approvals != 0 || h.Qualifies {
		t.Fatalf("empty marks = %+v", h)
	}
	// trust.ask_after_approvals = 0 turns the ask off: never qualifies.
	if h := countApprovals(approvals(50, histNow.Add(-24*time.Hour)), since(),
		0); h.Approvals != 50 || h.Qualifies {
		t.Fatalf("required 0 = %+v, want 50 counted, never qualifying", h)
	}
	if h := countApprovals(approvals(50, histNow.Add(-24*time.Hour)), since(),
		-3); h.Qualifies || h.Required != 0 {
		t.Fatalf("negative required = %+v, want disabled", h)
	}
}

func historyEvidence(class ActionClass, approvals int, floorMet bool) Evidence {
	start := histNow.Add(-60 * 24 * time.Hour)
	if !floorMet {
		start = histNow.Add(-time.Hour)
	}
	h := ApprovalHistory{Approvals: approvals, Required: 10, Qualifies: approvals >= 10}
	return Evidence{Family: SelfFamilyFor(class), Class: class, At: histNow,
		Record: &ClassRecord{}, ApprovalHistory: &h,
		Floor: &FloorStatus{Known: true, Start: start, Observed: histNow.Sub(start),
			RequiredL2: 192 * time.Hour, RequiredL3: 744 * time.Hour}}
}

func historyCheck(a Assessment, name string) *Check {
	for i := range a.Checks {
		if a.Checks[i].Name == name {
			return &a.Checks[i]
		}
	}
	return nil
}

func TestAssessApprovalHistory(t *testing.T) {
	a := AssessApprovalHistory(L3, historyEvidence(ClassIndexCreate, 12, true))
	if !a.Met || a.Target != L3 || historyCheck(a, "approval_history") == nil {
		t.Fatalf("12 approvals, floor passed: %+v, want met", a)
	}
	a = AssessApprovalHistory(L3, historyEvidence(ClassIndexCreate, 9, true))
	if c := historyCheck(a, "approval_history"); a.Met || c == nil || c.Met ||
		!strings.Contains(c.How, "1 more") {
		t.Fatalf("9 approvals: %+v, want unmet with 1 more to go", a)
	}
	a = AssessApprovalHistory(L3, historyEvidence(ClassIndexCreate, 12, false))
	if c := historyCheck(a, "observation_floor"); a.Met || c == nil || c.Met {
		t.Fatalf("floor not passed: %+v, want unmet", a)
	}
	// An irreversible class never qualifies, at any count.
	a = AssessApprovalHistory(L3, historyEvidence(ClassRetention, 500, true))
	if a.Met {
		t.Fatalf("irreversible class: %+v, want never met", a)
	}
	// Approval history is the L2 to L3 step only.
	if a := AssessApprovalHistory(L2, historyEvidence(ClassIndexCreate, 12,
		true)); a.Met {
		t.Fatalf("target L2: %+v, want unmet", a)
	}
	// No history read (an incident pair): unmet.
	ev := historyEvidence(ClassIndexCreate, 12, true)
	ev.ApprovalHistory = nil
	if a := AssessApprovalHistory(L3, ev); a.Met {
		t.Fatalf("no history: %+v, want unmet", a)
	}
}

// allowRow is a self-initiated row at level whose L3 floor has passed.
func allowRow(class ActionClass, level Level, approvals int) TrustRow {
	row := selfRow(class, level)
	row.Family = SelfFamilyFor(class)
	row.Cap = CapForPair(row.Family, class)
	h := ApprovalHistory{Approvals: approvals, Required: 10, Qualifies: approvals >= 10}
	row.ApprovalHistory = &h
	row.Next = &Assessment{Target: L3, Checks: []Check{{Name: "observation_floor",
		Met: true}}}
	return row
}

func TestAllowAlways_ShowsOnlyWhenEveryConditionHolds(t *testing.T) {
	ceil := fullCeiling()
	if ok, why := AllowAlways(allowRow(ClassIndexCreate, L2, 12), ceil); !ok {
		t.Fatalf("12 approvals at L2: hidden (%s), want shown", why)
	}
	cases := []struct {
		name string
		row  TrustRow
		ceil Ceiling
		why  string
	}{
		{"nine approvals", allowRow(ClassIndexCreate, L2, 9), ceil, "approv"},
		{"at L1", allowRow(ClassIndexCreate, L1, 12), ceil, "l2"},
		{"already L3", allowRow(ClassIndexCreate, L3, 12), ceil, "l2"},
		{"irreversible", allowRow(ClassRetention, L2, 500), ceil, "undone"},
		{"advisory moderate", allowRow(ClassIndexCreate, L2, 12),
			modeCeiling("advisory", "auto", true, true), "advisory"},
		{"approval mode", allowRow(ClassIndexCreate, L2, 12),
			modeCeiling("autonomous", "approval", true, true), "approval"},
		{"no executor", allowRow(ClassIndexCreate, L2, 12), Ceiling{}, "watch"},
		{"floor not passed", func() TrustRow {
			r := allowRow(ClassIndexCreate, L2, 12)
			r.Next.Checks[0].Met = false
			return r
		}(), ceil, "floor"},
		{"effective below L2", func() TrustRow {
			r := allowRow(ClassIndexCreate, L2, 12)
			l1 := L1
			r.Effective = &l1
			return r
		}(), ceil, "l2"},
		{"no history", func() TrustRow {
			r := allowRow(ClassIndexCreate, L2, 12)
			r.ApprovalHistory = nil
			return r
		}(), ceil, "approv"},
	}
	for _, tc := range cases {
		ok, why := AllowAlways(tc.row, tc.ceil)
		if ok || !strings.Contains(strings.ToLower(why), tc.why) {
			t.Errorf("%s: shown=%v why=%q, want hidden mentioning %q", tc.name, ok, why, tc.why)
		}
	}
}

func TestAllowAlways_APendingL3ProposalIsEnoughEvidence(t *testing.T) {
	row := allowRow(ClassIndexCreate, L2, 0)
	row.Pending = &Proposal{ID: "p1", From: L2, To: L3}
	if ok, why := AllowAlways(row, fullCeiling()); !ok {
		t.Fatalf("pending L3 proposal: hidden (%s), want shown", why)
	}
	row.Pending = &Proposal{ID: "p2", From: L1, To: L2}
	if ok, _ := AllowAlways(row, fullCeiling()); ok {
		t.Fatal("a pending L2 proposal must not show allow-always")
	}
	// An incident pair has no approval history; its pending L3 proposal
	// (full evidence) shows the button, the ramp read from the ceiling.
	inc := modeRow(KindIncident, ClassFreeze, L2)
	inc.Pending = &Proposal{ID: "p3", From: L2, To: L3}
	if ok, why := AllowAlways(inc, fullCeiling()); !ok {
		t.Fatalf("incident pending L3: hidden (%s)", why)
	}
	young := with(fullCeiling(), func(c *Ceiling) { c.RampStart = modeEpoch.Add(-time.Hour) })
	if ok, why := AllowAlways(inc, young); ok || !strings.Contains(why, "floor") {
		t.Fatalf("incident before the ramp: shown=%v (%s), want hidden by the floor", ok, why)
	}
}

func TestAllowAlways_SafeClassShowsUnderAdvisory(t *testing.T) {
	ceil := modeCeiling("advisory", "auto", true, false)
	if ok, why := AllowAlways(allowRow(ClassVacuum, L2, 10), ceil); !ok {
		t.Fatalf("safe class under advisory: hidden (%s), want shown", why)
	}
}

func TestApprovalAsk_SaysTheCountAndTheDatabase(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"kind": EvidenceKindApprovalHistory,
		"evidence": map[string]any{"approval_history": ApprovalHistory{Approvals: 14,
			Required: 10, Qualifies: true}}})
	p := Proposal{Family: FamilyHygiene, Class: ClassAnalyze, From: L2, To: L3, Evidence: raw}
	got, ok := ApprovalAsk(p, "orders")
	want := "You approved analyze 14 times on orders with no rollback; allow it unattended?"
	if !ok || got != want {
		t.Fatalf("ask = %q, %v; want %q", got, ok, want)
	}
	for _, bad := range []json.RawMessage{nil, []byte(`{}`), []byte(`not json`),
		[]byte(`{"evidence":{"approval_history":{"approvals":3,"qualifies":false}}}`)} {
		if got, ok := ApprovalAsk(Proposal{Class: ClassAnalyze, To: L3, Evidence: bad},
			"orders"); ok || got != "" {
			t.Errorf("evidence %s: ask %q, want none", bad, got)
		}
	}
}

func TestCheckHumanApprover(t *testing.T) {
	for _, actor := range []string{"user:7:ops@example.com", "user:1"} {
		if err := CheckHumanApprover(actor); err != nil {
			t.Errorf("%q: %v, want a person", actor, err)
		}
	}
	for _, actor := range []string{"", "pg_sage", "mcp:token:4", "mcp-agent:x", "stdio",
		"system", strings.Repeat("u", 201)} {
		if err := CheckHumanApprover(actor); !errors.Is(err, ErrHumanApprovalRequired) {
			t.Errorf("%q: %v, want ErrHumanApprovalRequired", actor, err)
		}
	}
}
