package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policy-01: a contract-less decision says so in its evidence, and an
// unclassified statement (no change class, so no feature) still records
// its withheld verdict instead of failing ledger validation.

func TestLedgerInputStampsContractNone(t *testing.T) {
	source := map[string]any{"pid": 42}
	request := policy.ActionRequest{Feature: "vacuum", SQL: "VACUUM FULL public.t",
		TargetObjs: []string{"public.t"}, Evidence: source}
	decision := policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonNoTypedContract, RiskTier: policy.RiskHigh}

	input := ledgerInputAt(nil, 1, request, decision)
	if input.Evidence["contract"] != "none" || input.RiskTier != "high" ||
		input.Reason != "no_typed_contract" || input.Verdict != ledger.VerdictPark {
		t.Fatalf("input = %#v, want contract=none at high", input)
	}
	if input.Evidence["pid"] != 42 {
		t.Fatalf("evidence = %#v, lost the request evidence", input.Evidence)
	}
	if _, mutated := source["contract"]; mutated {
		t.Fatalf("ledgerInput wrote into the request's evidence map: %#v", source)
	}
}

func TestLedgerInputOmitsContractNoneForTypedRequests(t *testing.T) {
	request := policy.ActionRequest{Feature: "analyze", SQL: "ANALYZE public.t",
		Contract: &policy.ActionContract{ActionType: "analyze_table", RiskTier: policy.RiskSafe}}
	input := ledgerInputAt(nil, 1, request, policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonTrustRampNotSatisfied, RiskTier: policy.RiskSafe})
	if _, ok := input.Evidence["contract"]; ok || input.RiskTier != "safe" {
		t.Fatalf("input = %#v, a typed request carries no contract marker", input)
	}
}

// TRUNCATE (and any statement the classifier does not know) has no change
// class, so the request's feature is empty. A withheld verdict records as
// the unclassified feature; the fingerprint follows it.
func TestLedgerInputNamesUnclassifiedWithheldFeature(t *testing.T) {
	request := policy.ActionRequest{SQL: "TRUNCATE public.t", TargetObjs: []string{"public.t"}}
	for _, verdict := range []policy.Verdict{policy.VerdictPark, policy.VerdictBlocked,
		policy.VerdictQueueApproval, policy.VerdictObserveOnly} {
		input := ledgerInputAt(nil, 1, request, policy.Decision{Verdict: verdict,
			Reason: policy.ReasonNoTypedContract, RiskTier: policy.RiskHigh})
		if input.Feature != unclassifiedFeature || input.Intent != unclassifiedFeature {
			t.Fatalf("%s: feature=%q intent=%q, want %q", verdict, input.Feature,
				input.Intent, unclassifiedFeature)
		}
		if input.Fingerprint != ledger.DecisionFingerprint(input) {
			t.Fatalf("%s: fingerprint not computed over the recorded feature", verdict)
		}
	}
}

// Safety: an execute verdict is never given a feature it did not have. It
// keeps failing ledger validation (fail closed) as before.
func TestLedgerInputKeepsEmptyFeatureOnExecute(t *testing.T) {
	request := policy.ActionRequest{SQL: "SELECT 1"}
	input := ledgerInputAt(nil, 1, request, policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonAuthorized, RiskTier: policy.RiskReadOnly})
	if input.Feature != "" || input.Intent != "" {
		t.Fatalf("execute input = %#v, want the empty feature kept", input)
	}
}
