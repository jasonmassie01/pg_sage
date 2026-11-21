package earned

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ux-07a on a real ledger: N person approvals with no rollback make
// pg_sage ask for L3 (Evaluate), "Not now" snoozes the ask for 30 days,
// and allow-always grants L3 through the ordinary proposal and approval,
// once, demoted like any earned level.

const humanApprover = "user:7:ops@example.com"

// newHistoryFixture is tuning/index_create at L2 (three improved
// approvals) with the ramp long passed.
func newHistoryFixture(t *testing.T) *fixture {
	t.Helper()
	return newHistoryFixtureFor(t, ClassIndexCreate)
}

// newHistoryFixtureFor is the tuning class at L2 (three improved
// approvals) with the ramp long passed.
func newHistoryFixtureFor(t *testing.T, class ActionClass) *fixture {
	t.Helper()
	f := newFixture(t)
	f.svc.cfg.AskAfterApprovals = DefaultAskAfterApprovals
	f.svc.WithRamp(func() RampFloor {
		return RampFloor{Start: f.clock.Now().Add(-60 * 24 * time.Hour)}
	})
	f.recordApproved(class, "improved", 3)
	if st := f.promote(FamilyTuning, class); st.Level != L2 {
		t.Fatalf("seeded level = %v, want L2", st.Level)
	}
	return f
}

var historyActionID int64 = 700000

// recordApproved records n person-approved (L2) executions of class
// with verdict, one minute apart.
func (f *fixture) recordApproved(class ActionClass, verdict string, n int) {
	f.t.Helper()
	family := SelfFamilyFor(class)
	for i := 0; i < n; i++ {
		historyActionID++
		result, _ := selfResult(family, verdict, "success")
		at := f.clock.Now()
		if err := f.svc.RecordOutcome(f.ctx, Outcome{Database: f.db,
			ActionLogID: historyActionID, Family: family, Class: class, Level: L2,
			Result: result, Source: SourceExecutor, Verdict: verdict, ObservedAt: &at,
			Actor: ActorPgSage}); err != nil {
			f.t.Fatalf("outcome: %v", err)
		}
		f.clock.Advance(time.Minute)
	}
}

// askFor is the pending L3 proposal of the pair created by Evaluate.
func (f *fixture) askFor(class ActionClass) *Proposal {
	f.t.Helper()
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		f.t.Fatalf("propose: %v", err)
	}
	p := f.pending(SelfFamilyFor(class), class)
	if p != nil && p.To != L3 {
		f.t.Fatalf("pending proposal %+v is not L3", p)
	}
	return p
}

func kindOf(t *testing.T, p Proposal) string {
	t.Helper()
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(p.Evidence, &body); err != nil {
		t.Fatalf("proposal evidence: %v", err)
	}
	return body.Kind
}

func TestApprovalHistory_NineApprovalsDoNotAskTenDo(t *testing.T) {
	f := newHistoryFixtureFor(t, ClassStatistics)
	f.recordApproved(ClassStatistics, "neutral", 6) // 3 improved + 6 neutral = 9
	if p := f.askFor(ClassStatistics); p != nil {
		t.Fatalf("9 approvals asked: %+v", p)
	}
	f.recordApproved(ClassStatistics, "neutral", 1)
	p := f.askFor(ClassStatistics)
	if p == nil || p.From != L2 || p.To != L3 {
		t.Fatalf("10 approvals: no L3 ask (%+v)", p)
	}
	if kind := kindOf(t, *p); kind != EvidenceKindApprovalHistory {
		t.Fatalf("proposal kind = %q, want approval_history", kind)
	}
	ask, ok := ApprovalAsk(*p, f.db)
	if !ok || !strings.Contains(ask, "You approved statistics 10 times on orders") {
		t.Fatalf("ask = %q %v", ask, ok)
	}
	// The ask is a proposal, never a grant.
	if got := f.granted(FamilyTuning, ClassStatistics); got != L2 {
		t.Fatalf("level after the ask = %v, want L2", got)
	}
}

// A neutral index create is reverted for no gain (Phase 1.3) without a
// rollback demerit: it did not hold, so it is never an approval, and
// twenty of them neither ask nor allow-always (review of ux-07a).
func TestApprovalHistory_NeutralIndexCreatesNeverAsk(t *testing.T) {
	f := newHistoryFixture(t)
	f.recordApproved(ClassIndexCreate, "neutral", 20)
	if p := f.askFor(ClassIndexCreate); p != nil {
		t.Fatalf("20 no-gain index creates asked: %+v", p)
	}
	ev, err := f.svc.decisionEvidence(f.ctx, FamilyTuning, ClassIndexCreate)
	if err != nil || ev.ApprovalHistory == nil || ev.ApprovalHistory.Approvals != 3 ||
		ev.ApprovalHistory.Neutral != 0 {
		t.Fatalf("history = %+v, %v; want the 3 improved only", ev.ApprovalHistory, err)
	}
	_, err = f.svc.AllowAlways(f.ctx, FamilyTuning, ClassIndexCreate, humanApprover, "x")
	if !errors.Is(err, ErrEvidenceNotMet) {
		t.Fatalf("allow always on no-gain index creates: %v, want ErrEvidenceNotMet", err)
	}
	if got := f.granted(FamilyTuning, ClassIndexCreate); got != L2 {
		t.Fatalf("level = %v, want L2", got)
	}
}

func TestApprovalHistory_ZeroDisablesTheAsk(t *testing.T) {
	f := newHistoryFixtureFor(t, ClassStatistics)
	f.svc.cfg.AskAfterApprovals = 0
	f.recordApproved(ClassStatistics, "neutral", 20)
	if p := f.askFor(ClassStatistics); p != nil {
		t.Fatalf("ask_after_approvals 0 asked: %+v", p)
	}
}

func TestApprovalHistory_NotNowSnoozesTheAskFor30Days(t *testing.T) {
	f := newHistoryFixtureFor(t, ClassStatistics)
	f.recordApproved(ClassStatistics, "neutral", 7)
	p := f.askFor(ClassStatistics)
	if p == nil {
		t.Fatal("no ask")
	}
	if _, err := f.svc.Reject(f.ctx, p.ID, humanApprover, "not now"); err != nil {
		t.Fatal(err)
	}
	f.recordApproved(ClassStatistics, "neutral", 5)
	if again := f.askFor(ClassStatistics); again != nil {
		t.Fatalf("asked again inside the snooze: %+v", again)
	}
	f.clock.Advance(29 * 24 * time.Hour)
	f.recordApproved(ClassStatistics, "neutral", 10)
	if again := f.askFor(ClassStatistics); again != nil {
		t.Fatalf("asked again at 29 days: %+v", again)
	}
	f.clock.Advance(2 * 24 * time.Hour)
	if again := f.askFor(ClassStatistics); again == nil ||
		kindOf(t, *again) != EvidenceKindApprovalHistory {
		t.Fatalf("no ask after the snooze: %+v", again)
	}
}

func TestAllowAlways_GrantsL3AndTheClassActsInTheWindow(t *testing.T) {
	f := newHistoryFixture(t)
	f.recordApproved(ClassIndexCreate, "improved", 9) // 12 approvals in all
	st, err := f.svc.AllowAlways(f.ctx, FamilyTuning, ClassIndexCreate, humanApprover,
		"allow always from card 41")
	if err != nil || st.Level != L3 {
		t.Fatalf("allow always = %+v, %v; want L3", st, err)
	}
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ceil := fullCeiling()
	ceil.Now = f.clock.Now()
	ceil.RampStart = f.clock.Now().Add(-60 * 24 * time.Hour)
	AnnotateMode(&v, ceil)
	row, ok := trustRow(v, FamilyTuning, ClassIndexCreate)
	if !ok || row.Level != L3 || row.Mode != ModeActsInWindow || row.Pending != nil {
		t.Fatalf("row after allow always = %+v", row)
	}
	evs := f.events(EventFilter{Family: FamilyTuning, Class: ClassIndexCreate,
		Database: f.db, Limit: 20})
	var approved, proposed *Event
	for i := range evs {
		switch {
		case evs[i].Type == EventPromotionApproved && evs[i].To != nil && *evs[i].To == L3:
			approved = &evs[i]
		case evs[i].Type == EventPromotionProposed && evs[i].To != nil && *evs[i].To == L3:
			proposed = &evs[i]
		}
	}
	if approved == nil || approved.Actor != humanApprover ||
		approved.Reason != "allow always from card 41" {
		t.Fatalf("approval event = %+v", approved)
	}
	if proposed == nil || !strings.Contains(proposed.Reason, "approval history") {
		t.Fatalf("proposal event = %+v", proposed)
	}
}

func TestAllowAlways_UsesAPendingProposal(t *testing.T) {
	f := newHistoryFixtureFor(t, ClassStatistics)
	f.recordApproved(ClassStatistics, "neutral", 8)
	p := f.askFor(ClassStatistics)
	if p == nil {
		t.Fatal("no ask")
	}
	if _, err := f.svc.AllowAlways(f.ctx, FamilyTuning, ClassStatistics, humanApprover,
		"allow always from card 3"); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Proposal(f.ctx, p.ID)
	if err != nil || got.Status != StatusApproved || got.DecidedBy != humanApprover {
		t.Fatalf("waiting proposal = %+v, %v; want approved by the person", got, err)
	}
}

func TestAllowAlways_RefusesAnAgentALongNoteAndThinEvidence(t *testing.T) {
	f := newHistoryFixture(t)
	f.recordApproved(ClassIndexCreate, "improved", 9)
	for _, agent := range []string{"mcp:token:4", ActorPgSage, ""} {
		_, err := f.svc.AllowAlways(f.ctx, FamilyTuning, ClassIndexCreate, agent, "x")
		if !errors.Is(err, ErrHumanApprovalRequired) {
			t.Fatalf("approver %q: %v, want ErrHumanApprovalRequired", agent, err)
		}
	}
	_, err := f.svc.AllowAlways(f.ctx, FamilyTuning, ClassIndexCreate, humanApprover,
		strings.Repeat("n", 2001))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("long note: %v, want ErrInvalidRequest", err)
	}
	if p := f.pending(FamilyTuning, ClassIndexCreate); p != nil {
		t.Fatalf("a refused allow-always left a proposal: %+v", p)
	}
	thin := newHistoryFixture(t) // 3 approvals
	_, err = thin.svc.AllowAlways(thin.ctx, FamilyTuning, ClassIndexCreate, humanApprover,
		"x")
	if !errors.Is(err, ErrEvidenceNotMet) {
		t.Fatalf("3 approvals: %v, want ErrEvidenceNotMet", err)
	}
	for _, fx := range []*fixture{f, thin} {
		if got := fx.granted(FamilyTuning, ClassIndexCreate); got != L2 {
			t.Fatalf("level after refusals = %v, want L2", got)
		}
	}
	// A class not at L2 (here L1) and an incident class without a
	// waiting L3 proposal are refused, never skipping a level.
	if _, err := f.svc.AllowAlways(f.ctx, FamilyHygiene, ClassAnalyze, humanApprover,
		"x"); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrEvidenceNotMet) {
		t.Fatalf("L1 class: %v, want a refusal", err)
	}
	if _, err := f.svc.AllowAlways(f.ctx, FamilyWraparound, ClassFreeze, humanApprover,
		"x"); err == nil {
		t.Fatal("incident class without a proposal was promoted")
	}
}

func TestAllowAlways_TwoPeopleAtOnceApproveOnce(t *testing.T) {
	f := newHistoryFixture(t)
	f.recordApproved(ClassIndexCreate, "improved", 9)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.AllowAlways(f.ctx, FamilyTuning, ClassIndexCreate,
				humanApprover, "allow always from card "+string(rune('1'+i)))
		}(i)
	}
	wg.Wait()
	ok, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotPending), errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatalf("results %v: want one approval and one conflict", errs)
	}
	n := 0
	for _, e := range f.events(EventFilter{Family: FamilyTuning, Class: ClassIndexCreate,
		Database: f.db, Limit: 50}) {
		if e.Type == EventPromotionApproved && e.To != nil && *e.To == L3 {
			n++
		}
	}
	if n != 1 || f.granted(FamilyTuning, ClassIndexCreate) != L3 {
		t.Fatalf("L3 approvals = %d, level %v; want exactly one, L3", n,
			f.granted(FamilyTuning, ClassIndexCreate))
	}
}

func TestAllowAlways_ARegressionDemotesAndTheButtonWaitsForNewEvidence(t *testing.T) {
	f := newHistoryFixture(t)
	f.recordApproved(ClassIndexCreate, "improved", 9)
	if _, err := f.svc.AllowAlways(f.ctx, FamilyTuning, ClassIndexCreate, humanApprover,
		"allow always from card 5"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	historyActionID++
	at := f.clock.Now()
	if err := f.svc.RecordOutcome(f.ctx, Outcome{Database: f.db,
		ActionLogID: historyActionID, Family: FamilyTuning, Class: ClassIndexCreate,
		Level: L3, Result: ResultHarmful, Source: SourceExecutor, Verdict: CauseRegressed,
		ObservedAt: &at, Actor: ActorPgSage}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	if got := f.granted(FamilyTuning, ClassIndexCreate); got != L2 {
		t.Fatalf("level after a regression = %v, want L2", got)
	}
	ceil := fullCeiling()
	ceil.Now = f.clock.Now()
	ceil.RampStart = f.clock.Now().Add(-60 * 24 * time.Hour)
	shown := func() (bool, string) {
		v, err := f.svc.TrustView(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := trustRow(v, FamilyTuning, ClassIndexCreate)
		return AllowAlways(row, ceil)
	}
	if ok, why := shown(); ok {
		t.Fatalf("button shown right after the regression (%s)", why)
	}
	f.recordApproved(ClassIndexCreate, "improved", 9)
	if ok, _ := shown(); ok {
		t.Fatal("button shown with 9 approvals since the regression")
	}
	f.recordApproved(ClassIndexCreate, "improved", 1)
	if ok, why := shown(); !ok {
		t.Fatalf("button hidden after the evidence re-qualified (%s)", why)
	}
}

// An incident pair granted L3 through allow-always decays like any
// earned level (earned-06b): warned 7 days ahead, capped at holds_until.
func TestAllowAlways_AnIncidentGrantDecaysAtHoldsUntil(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL2Evidence(FamilyWraparound, 25, 0)
	lf.promote(FamilyWraparound, ClassFreeze)
	lf.seedL2Recoveries(FamilyWraparound, ClassFreeze, 50)
	if _, err := lf.svc.ProposePromotions(lf.ctx); err != nil {
		t.Fatal(err)
	}
	// An incident class is never asked from approvals: its L3 proposal
	// rests on the full incident evidence.
	if p := lf.pending(FamilyWraparound, ClassFreeze); p == nil || p.To != L3 ||
		kindOf(t, *p) != "" {
		t.Fatalf("incident L3 proposal = %+v", p)
	}
	st, err := lf.svc.AllowAlways(lf.ctx, FamilyWraparound, ClassFreeze, humanApprover,
		"allow always from card 8")
	if err != nil || st.Level != L3 {
		t.Fatalf("allow always = %+v, %v", st, err)
	}
	lf.clock.Set(fixtureEpoch.Add(10 * day))
	for i := 0; i < 20; i++ {
		if err := lf.svc.RecordReview(lf.ctx, Review{Database: lf.db,
			InvestigationID: newUUID(t), Family: FamilyWraparound, Verdict: VerdictAccepted,
			Reviewer: humanApprover}); err != nil {
			t.Fatal(err)
		}
	}
	n := &expiryRecorder{}
	lf.clock.Set(decayGenerated.Add(23*day + time.Minute))
	if res := lf.sweep(n); res.Warned != 1 || n.count() != 1 ||
		!n.warns[0].HoldsUntil.Equal(decayGenerated.Add(30*day)) {
		t.Fatalf("7 days ahead: %+v, warnings %+v", res, n.warns)
	}
	lf.clock.Set(decayGenerated.Add(30*day + time.Minute))
	if res := lf.sweep(nil); res.Capped != 1 {
		t.Fatalf("at holds_until: %+v, want a cap", res)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 1 || got.Granted != 3 {
		t.Fatalf("limit after decay = %+v, want L1 of granted L3", got)
	}
}
