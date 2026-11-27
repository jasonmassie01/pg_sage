package schemaguard

import (
	"strings"
	"testing"
)

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
			name: "everything text", kind: InvariantEverythingText,
			wantClass: RemediationStructural, rehearse: true,
		},
		{
			name: "type tightening", kind: InvariantTypeTightening,
			wantClass: RemediationStructural, rehearse: true,
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

// Kinds without a detector were removed (migration-09): they read as
// unknown, so nothing can plan, route or auto-apply them.
func TestClassifyInvariantRejectsKindsWithoutADetector(t *testing.T) {
	for _, kind := range []InvariantKind{"redundant_index", "random_uuid_pk",
		"no_primary_key", "missing_constraint", ""} {
		t.Run(string(kind), func(t *testing.T) {
			classification, err := ClassifyInvariant(kind)
			if err == nil || !strings.Contains(err.Error(), "unknown invariant kind") {
				t.Fatalf("ClassifyInvariant(%q) err = %v, want unknown-kind error", kind, err)
			}
			if classification != (Classification{}) {
				t.Fatalf("classification = %#v, want zero", classification)
			}
		})
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
