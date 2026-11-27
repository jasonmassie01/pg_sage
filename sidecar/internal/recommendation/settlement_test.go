package recommendation

import "testing"

// No concurrent access tests here: settlement is a pure function of one
// effect value. The race it guards against (a verification row that is
// linked but not yet visible) is covered on real PostgreSQL in
// recover_test.go.

func strp(s string) *string { return &s }

type settlementCase struct {
	name         string
	outcome      *string
	verification *string
	expected     bool // a verification row is linked or about to land
	prior        string
	verdict      string
	final        State
}

// successSettlementCases cover a nil outcome and outcome success by verdict.
func successSettlementCases() []settlementCase {
	return []settlementCase{
		{name: "no action outcome", verdict: "missing", final: StateInconclusive},
		{name: "no action outcome ignores a success row",
			verification: strp("success"), verdict: "missing", final: StateInconclusive},
		{name: "success without verifier evidence (post-check action)",
			outcome: strp("success"), verdict: "success", final: StateVerified},
		{name: "success with a linked row not yet visible keeps verifying",
			outcome: strp("success"), expected: true, prior: "", verdict: "", final: ""},
		{name: "success with a linked row keeps the prior verdict",
			outcome: strp("success"), expected: true, prior: "regressed",
			verdict: "regressed", final: ""},
		{name: "success verified by the verifier",
			outcome: strp("success"), verification: strp("success"), expected: true,
			verdict: "success", final: StateVerified},
		{name: "neutral success is inconclusive",
			outcome: strp("success"), verification: strp("unverifiable"), expected: true,
			verdict: "unverifiable", final: StateInconclusive},
		{name: "success with a failed verification is inconclusive",
			outcome: strp("success"), verification: strp("failed"), expected: true,
			verdict: "failed", final: StateInconclusive},
		{name: "success with a revert verdict keeps verifying as regressed",
			outcome: strp("success"), verification: strp("revert"), expected: true,
			verdict: "regressed", final: ""},
		{name: "success while verification is pending keeps verifying",
			outcome: strp("success"), verification: strp("pending"), expected: true,
			verdict: "", final: ""},
		{name: "success while verification is extended keeps verifying",
			outcome: strp("success"), verification: strp("extended"), expected: true,
			prior: "unverifiable", verdict: "unverifiable", final: ""},
		{name: "success with an unknown verdict fails closed (keeps verifying)",
			outcome: strp("success"), verification: strp("bogus"), expected: true,
			verdict: "", final: ""},
	}
}

// otherSettlementCases are the non-success outcomes, unchanged by cases-03.
func otherSettlementCases() []settlementCase {
	return []settlementCase{
		{name: "rolled back ignores a success row",
			outcome: strp("rolled_back"), verification: strp("success"),
			verdict: "regressed", final: StateReverted},
		{name: "rolled back without a row", outcome: strp("rolled_back"),
			verdict: "regressed", final: StateReverted},
		{name: "unverifiable outcome", outcome: strp("unverifiable"),
			verdict: "unverifiable", final: StateInconclusive},
		{name: "failed outcome", outcome: strp("failed"),
			verdict: "failed", final: StateInconclusive},
		{name: "rollback failed", outcome: strp("rollback_failed"),
			verification: strp("success"), verdict: "regressed", final: ""},
		{name: "rollback skipped", outcome: strp("rollback_skipped"),
			verdict: "regressed", final: ""},
		{name: "rollback withheld", outcome: strp("rollback_withheld"),
			verdict: "regressed", final: ""},
		{name: "monitoring without a row", outcome: strp("monitoring"),
			prior: "x", verdict: "x", final: ""},
		{name: "monitoring with a revert verdict", outcome: strp("monitoring"),
			verification: strp("revert"), verdict: "regressed", final: ""},
		{name: "monitoring with an unverifiable verdict", outcome: strp("monitoring"),
			verification: strp("unverifiable"), verdict: "unverifiable", final: ""},
		{name: "monitoring with a success verdict waits for the outcome",
			outcome: strp("monitoring"), verification: strp("success"),
			verdict: "", final: ""},
		{name: "empty outcome string", outcome: strp(""), verdict: "", final: ""},
	}
}

func TestSettlementTable(t *testing.T) {
	for _, tc := range append(successSettlementCases(), otherSettlementCases()...) {
		e := effect{verdict: tc.prior, outcome: tc.outcome,
			verification: tc.verification, verificationExpected: tc.expected}
		verdict, final := settlement(e)
		if verdict != tc.verdict || final != tc.final {
			t.Errorf("%s: settlement = (%q, %q), want (%q, %q)",
				tc.name, verdict, final, tc.verdict, tc.final)
		}
	}
}

// A success outcome with a durable unverifiable verdict (the executor's
// neutral verdict) is inconclusive, never verified (cases-03).
func TestSettlementNeutralSuccessIsInconclusive(t *testing.T) {
	verdict, final := settlement(effect{outcome: strp("success"),
		verification: strp("unverifiable"), verificationExpected: true})
	if final != StateInconclusive || verdict != "unverifiable" {
		t.Fatalf("neutral success settled as (%q, %q), want (unverifiable, %s)",
			verdict, final, StateInconclusive)
	}
}

// State transition: the head keeps verifying while its verification is
// pending, then reaches verified when the verifier records success.
func TestSettlementPendingThenSuccess(t *testing.T) {
	e := effect{outcome: strp("success"), verification: strp("pending"),
		verificationExpected: true}
	if verdict, final := settlement(e); final != "" || verdict != "" {
		t.Fatalf("pending verification: (%q, %q), want to keep verifying", verdict, final)
	}
	e.verification = strp("success")
	if verdict, final := settlement(e); final != StateVerified || verdict != "success" {
		t.Fatalf("after success: (%q, %q), want (success, %s)", verdict, final,
			StateVerified)
	}
}

// Zero value: an effect read for a head with no action at all.
func TestSettlementZeroEffect(t *testing.T) {
	if verdict, final := settlement(effect{}); final != StateInconclusive ||
		verdict != "missing" {
		t.Fatalf("zero effect: (%q, %q), want (missing, %s)", verdict, final,
			StateInconclusive)
	}
}
