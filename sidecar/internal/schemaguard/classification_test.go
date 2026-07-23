package schemaguard

import "testing"

func TestClassifyInvariantUsesPrescribedRemediationClasses(t *testing.T) {
	tests := []struct {
		name         string
		kind         InvariantKind
		wantClass    RemediationClass
		auto         bool
		verify       bool
		rehearse     bool
		needsConsent bool
	}{
		{
			name: "missing FK index", kind: InvariantMissingFKIndex,
			wantClass: RemediationVerifiedIndex, auto: true, verify: true,
		},
		{
			name: "unbounded append table", kind: InvariantUnboundedAppend,
			wantClass: RemediationRetention, auto: true, needsConsent: true,
		},
		{
			name: "missing constraint", kind: InvariantMissingConstraint,
			wantClass: RemediationRecommendation,
		},
		{
			name: "everything text", kind: InvariantEverythingText,
			wantClass: RemediationStructural, rehearse: true,
		},
		{
			name: "type tightening", kind: InvariantTypeTightening,
			wantClass: RemediationStructural, rehearse: true,
		},
		{
			name: "random UUID primary key", kind: InvariantRandomUUIDPK,
			wantClass: RemediationStructural, rehearse: true,
		},
		{
			name: "no primary key", kind: InvariantNoPrimaryKey,
			wantClass: RemediationRecommendation,
		},
		{
			name: "redundant index", kind: InvariantRedundantIndex,
			wantClass: RemediationVerifiedIndex, auto: true, verify: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ClassifyInvariant(test.kind)
			if err != nil {
				t.Fatalf("ClassifyInvariant: %v", err)
			}
			if got.Class != test.wantClass || got.AutoRemediable != test.auto ||
				got.RequiresVerification != test.verify ||
				got.RequiresRehearsal != test.rehearse ||
				got.RequiresPolicyConsent != test.needsConsent {
				t.Fatalf("classification = %#v", got)
			}
		})
	}
}

func TestClassifyInvariantRejectsUnknownKind(t *testing.T) {
	classification, err := ClassifyInvariant(InvariantKind("llm_guess"))
	if err == nil {
		t.Fatalf("classification = %#v, want unknown-kind error", classification)
	}
	if classification.AutoRemediable {
		t.Fatalf("unknown invariant became auto-remediable: %#v", classification)
	}
}

func TestDefaultPolicyUsesExistingOscillationLimit(t *testing.T) {
	policy := DefaultPolicy()
	if policy.OscillationLimit != 3 {
		t.Fatalf("oscillation limit = %d, want 3", policy.OscillationLimit)
	}
}

// No concurrent-access test: classification is stateless and only consumes values.
// No integration test: orchestration emits decisions and performs no database I/O.
