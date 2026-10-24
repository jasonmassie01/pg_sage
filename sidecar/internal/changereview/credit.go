package changereview

import "time"

// ToilModel is the unit of credit: sage.toil_model's active
// 'change_review' row (model 1: 15 minutes).
type ToilModel struct {
	Version int `json:"version"`
	Minutes int `json:"minutes"`
}

// Credit is a review's value-ledger state: none, pending (a DBA concurred;
// Minutes and ToilModelVersion stamped), final (the receipt came back
// clean; FinalAt set) or void (the receipt regressed or was unverifiable,
// or the change never landed). Only final minutes count as hours.
type Credit struct {
	State            string     `json:"state"`
	Minutes          int        `json:"minutes"`
	ToilModelVersion int        `json:"toil_model_version,omitempty"`
	FinalAt          *time.Time `json:"final_at,omitempty"`
}

// CreditFacts are what the credit depends on. Carried marks a verdict
// carried over from an earlier revision: the credit belongs to the review
// the DBA decided, so a carried review earns none.
type CreditFacts struct {
	DBAVerdict     string
	ReceiptVerdict string
	LandingState   string
	Carried        bool
}

// NextCredit is the pure credit state machine:
//
//	final, void                         → unchanged (terminal)
//	carried verdict                     → none
//	landing not_applicable              → none (nothing lands; no receipt)
//	concur_with_edits, override         → none
//	receipt regressed or unverifiable,
//	or landing expired                  → void
//	concur, receipt clean[_low_traffic] → final (FinalAt = now)
//	concur otherwise                    → pending
//	no verdict                          → none
//
// It never reads shadow auto-clear fields.
func NextCredit(cur Credit, f CreditFacts, toil ToilModel, now time.Time) Credit {
	switch cur.State {
	case CreditFinal, CreditVoid:
		return cur
	}
	if f.Carried || f.LandingState == LandingNotApplicable {
		return Credit{State: CreditNone}
	}
	switch f.DBAVerdict {
	case DBAConcurWithEdits, DBAOverride:
		return Credit{State: CreditNone}
	}
	if f.ReceiptVerdict == ReceiptRegressed || f.ReceiptVerdict == ReceiptUnverifiable ||
		f.LandingState == LandingExpired {
		return Credit{State: CreditVoid}
	}
	if f.DBAVerdict != DBAConcur {
		return Credit{State: CreditNone}
	}
	c := Credit{State: CreditPending, Minutes: toil.Minutes, ToilModelVersion: toil.Version}
	if f.ReceiptVerdict == ReceiptClean || f.ReceiptVerdict == ReceiptCleanLowTraffic {
		at := now.UTC().Truncate(time.Microsecond)
		c.State, c.FinalAt = CreditFinal, &at
	}
	return c
}
