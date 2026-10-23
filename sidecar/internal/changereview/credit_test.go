package changereview

import (
	"testing"
	"time"
)

// The credit state machine (credit.go) is pure: concur → pending with the
// toil model's minutes and version; a clean receipt → final with
// credit_final_at; concur_with_edits or override → none; a regressed or
// unverifiable receipt, or an expired landing → void. final and void are
// terminal; a carried-over verdict earns nothing (the credit belongs to
// the review the DBA decided).

var toil = ToilModel{Version: 1, Minutes: 15}

func TestNextCreditTable(t *testing.T) {
	now := utc(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	earlier := now.Add(-time.Hour)
	none := Credit{State: CreditNone}
	pending := Credit{State: CreditPending, Minutes: 15, ToilModelVersion: 1}
	final := Credit{State: CreditFinal, Minutes: 15, ToilModelVersion: 1, FinalAt: &earlier}
	void := Credit{State: CreditVoid}
	cases := []struct {
		name string
		cur  Credit
		f    CreditFacts
		want Credit
	}{
		{"no verdict yet", none, CreditFacts{ReceiptVerdict: ReceiptPending,
			LandingState: LandingAwaiting}, none},
		{"concur", none, CreditFacts{DBAVerdict: DBAConcur, ReceiptVerdict: ReceiptPending,
			LandingState: LandingAwaiting}, pending},
		{"concur after clean", none, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptClean, LandingState: LandingLanded},
			Credit{State: CreditFinal, Minutes: 15, ToilModelVersion: 1, FinalAt: &now}},
		{"clean", pending, CreditFacts{DBAVerdict: DBAConcur, ReceiptVerdict: ReceiptClean,
			LandingState: LandingLanded},
			Credit{State: CreditFinal, Minutes: 15, ToilModelVersion: 1, FinalAt: &now}},
		{"clean low traffic", pending, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptCleanLowTraffic, LandingState: LandingLanded},
			Credit{State: CreditFinal, Minutes: 15, ToilModelVersion: 1, FinalAt: &now}},
		{"edits", none, CreditFacts{DBAVerdict: DBAConcurWithEdits,
			ReceiptVerdict: ReceiptClean, LandingState: LandingLanded}, none},
		{"override", none, CreditFacts{DBAVerdict: DBAOverride,
			ReceiptVerdict: ReceiptPending, LandingState: LandingAwaiting}, none},
		{"regressed", pending, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptRegressed, LandingState: LandingLanded}, void},
		{"unverifiable", pending, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptUnverifiable, LandingState: LandingLanded}, void},
		{"expired", pending, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptUnverifiable, LandingState: LandingExpired}, void},
		{"regressed before verdict", none, CreditFacts{ReceiptVerdict: ReceiptRegressed,
			LandingState: LandingLanded}, void},
		{"final is terminal", final, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptRegressed, LandingState: LandingLanded}, final},
		{"void is terminal", void, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptClean, LandingState: LandingLanded}, void},
		{"carried verdict", none, CreditFacts{DBAVerdict: DBAConcur, Carried: true,
			ReceiptVerdict: ReceiptClean, LandingState: LandingLanded}, none},
		{"not applicable landing", none, CreditFacts{DBAVerdict: DBAConcur,
			ReceiptVerdict: ReceiptPending, LandingState: LandingNotApplicable}, pending},
	}
	for _, c := range cases {
		got := NextCredit(c.cur, c.f, toil, now)
		if !creditEqual(got, c.want) {
			t.Errorf("%s: NextCredit = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func creditEqual(a, b Credit) bool {
	if a.State != b.State || a.Minutes != b.Minutes || a.ToilModelVersion != b.ToilModelVersion {
		return false
	}
	if (a.FinalAt == nil) != (b.FinalAt == nil) {
		return false
	}
	return a.FinalAt == nil || a.FinalAt.Equal(*b.FinalAt)
}

func TestNextCreditUsesTheToilModel(t *testing.T) {
	got := NextCredit(Credit{State: CreditNone}, CreditFacts{DBAVerdict: DBAConcur,
		ReceiptVerdict: ReceiptPending, LandingState: LandingAwaiting},
		ToilModel{Version: 3, Minutes: 22}, time.Now())
	if got.Minutes != 22 || got.ToilModelVersion != 3 {
		t.Fatalf("credit = %+v, want 22 minutes of toil model 3", got)
	}
}

func TestNextCreditEmptyStateIsNone(t *testing.T) {
	got := NextCredit(Credit{}, CreditFacts{}, toil, time.Now())
	if got.State != CreditNone || got.Minutes != 0 {
		t.Fatalf("zero credit = %+v, want none", got)
	}
}
