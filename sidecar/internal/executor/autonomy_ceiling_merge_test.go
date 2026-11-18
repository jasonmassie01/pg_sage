package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-06b review: a ledger class can hold several action types, and the
// page shows one sentence for the class. The merged contract must never
// let the class claim to act unattended where one of its action types
// would not: the sentence is only ever an upper bound through the
// standing document, never through the merge.

// No concurrent access test: AutonomyClassContracts returns a copy of a
// map built once at package init.

func TestAutonomyClassContracts_MixedClassKeepsTheStrictestRule(t *testing.T) {
	got := AutonomyClassContracts()[earned.ClassQueryHint]
	// apply_query_hint is MODERATE with approval_required and
	// retire_query_hint SAFE without it: the class keeps both the higher
	// tier and the approval rule, whatever order the types are listed in.
	want := earned.ClassContract{RiskTier: policy.RiskModerate,
		Rollback: policy.RollbackReversible, ApprovalRequired: true,
		ChangeClass: policy.ChangeQueryHint}
	if got != want {
		t.Fatalf("query_hint = %+v, want %+v", got, want)
	}
}

func acting(m earned.ModeName) bool {
	return m == earned.ModeActsThenTells || m == earned.ModeActsInWindow
}

// mergeCeilings is every operator setting that can change a verdict,
// with the ramp long elapsed and a window configured.
func mergeCeilings() []earned.Ceiling {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	var out []earned.Ceiling
	for _, trust := range []string{"observation", "advisory", "autonomous"} {
		for _, mode := range []string{"manual", "approval", "auto"} {
			for flags := 0; flags < 16; flags++ {
				out = append(out, earned.Ceiling{GateAttached: true, ExecutorEnabled: true,
					TrustLevel: trust, ExecutionMode: mode, Tier3Safe: flags&1 != 0,
					Tier3Moderate: flags&2 != 0, LedgerEnforced: flags&4 != 0,
					SQLValidationDegraded: flags&8 != 0, WindowConfigured: true,
					RampStart: now.Add(-90 * 24 * time.Hour), Now: now})
			}
		}
	}
	return out
}

func TestAutonomyClassContracts_ClassNeverActsWhereOneTypeWouldNot(t *testing.T) {
	merged, checked := AutonomyClassContracts(), 0
	for _, spec := range earned.Classes() {
		for _, actionType := range spec.ActionTypes {
			contract, ok := ContractForActionType(actionType)
			if !ok {
				continue
			}
			own := mergeClassContract(earned.ClassContract{}, false, contract)
			for _, ceil := range mergeCeilings() {
				for level := earned.L0; level <= earned.L3; level++ {
					for _, kind := range []string{earned.KindSelfInitiated, earned.KindIncident} {
						row := earned.TrustRow{Kind: kind, Class: spec.Class, Level: level,
							Cap: earned.L3}
						ceil.Contracts = merged
						classMode := earned.ModeOf(row, ceil).Name
						ceil.Contracts = map[earned.ActionClass]earned.ClassContract{
							spec.Class: own}
						typeMode := earned.ModeOf(row, ceil).Name
						checked++
						if acting(classMode) && !acting(typeMode) {
							t.Errorf("%s says %s, but its %s %s under trust=%s mode=%s "+
								"safe=%v moderate=%v", spec.Class, classMode, actionType,
								typeMode, ceil.TrustLevel, ceil.ExecutionMode, ceil.Tier3Safe,
								ceil.Tier3Moderate)
						}
					}
				}
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("checked %d class/type combinations, want at least 1000", checked)
	}
}
