package approvalcard

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-07a: the card offers "Approve, and stop asking for <class> on
// <database>" (allow_always) only when the server's rule holds; the
// ceiling it is computed against is read once per request. Without a
// ceiling (no executor) or a ledger the button is never offered.

// No concurrent access tests: trustOf is a pure function of the inputs.

func allowCeiling() *earned.Ceiling {
	return &earned.Ceiling{GateAttached: true, ExecutorEnabled: true,
		TrustLevel: policy.TrustAdvisory, ExecutionMode: policy.ExecutionAuto,
		Tier3Safe: true, WindowConfigured: true, LedgerEnforced: true,
		RampStart: now.Add(-60 * 24 * time.Hour), Now: now,
		Contracts: map[earned.ActionClass]earned.ClassContract{
			earned.ClassAnalyze: {RiskTier: policy.RiskSafe,
				Rollback: policy.RollbackNoRollbackNeeded, ChangeClass: policy.ChangeAnalyze},
		}}
}

func allowAlwaysRow(approvals int) *earned.TrustRow {
	row := analyzeTrustRow()
	row.Reversibility = earned.Reversible
	row.Next.Checks[2].Met = true // the L3 floor has passed
	row.ApprovalHistory = &earned.ApprovalHistory{Approvals: approvals, Required: 10,
		Qualifies: approvals >= 10}
	return row
}

func TestCardOffersAllowAlwaysWhenTheRuleHolds(t *testing.T) {
	in := analyzeInputs()
	in.Trust, in.Ceiling = allowAlwaysRow(12), allowCeiling()
	tr := Assemble(in).Trust
	if tr == nil || !tr.AllowAlways || tr.ApprovalHistory == nil ||
		tr.ApprovalHistory.Approvals != 12 {
		t.Fatalf("trust = %+v, want allow_always with 12 approvals", tr)
	}
	if !strings.Contains(tr.AllowAlwaysLabel, "stop asking for analyze on orders") {
		t.Fatalf("label = %q", tr.AllowAlwaysLabel)
	}
}

func TestCardHidesAllowAlways(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Inputs)
	}{
		{"nine approvals", func(in *Inputs) { in.Trust = allowAlwaysRow(9) }},
		{"no ceiling", func(in *Inputs) { in.Ceiling = nil }},
		{"no ledger", func(in *Inputs) { in.Trust = nil }},
		{"execution mode approval", func(in *Inputs) {
			c := allowCeiling()
			c.ExecutionMode = policy.ExecutionApproval
			in.Ceiling = c
		}},
	}
	for _, tc := range cases {
		in := analyzeInputs()
		in.Trust, in.Ceiling = allowAlwaysRow(12), allowCeiling()
		tc.edit(&in)
		tr := Assemble(in).Trust
		if tr != nil && (tr.AllowAlways || tr.AllowAlwaysLabel != "") {
			t.Errorf("%s: trust = %+v, want no allow_always", tc.name, tr)
		}
		if tc.name != "no ledger" && (tr == nil || tr.AllowAlwaysWhy == "") {
			t.Errorf("%s: no reason given for the hidden button: %+v", tc.name, tr)
		}
	}
}

func TestAllowAlwaysIsNotPartOfTheCardHash(t *testing.T) {
	in := analyzeInputs()
	in.Trust, in.Ceiling = allowAlwaysRow(12), allowCeiling()
	shown := Assemble(in)
	in.Trust = allowAlwaysRow(3)
	hidden := Assemble(in)
	if !shown.Trust.AllowAlways || hidden.Trust.AllowAlways ||
		shown.CardHash != hidden.CardHash {
		t.Fatalf("hash %q vs %q: the button must not change the content hash",
			shown.CardHash, hidden.CardHash)
	}
}
