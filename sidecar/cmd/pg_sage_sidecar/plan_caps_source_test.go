package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// plan-02b: the plan engine reads its caps from the plans block of the
// standing policy. No concurrent access test: the source holds no state
// beyond the read it wraps.

func storedPolicy(t *testing.T, doc policy.Document) policy.Policy {
	t.Helper()
	raw, err := policy.MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	return policy.Policy{Version: 3, Profile: "staffed", Document: json.RawMessage(raw)}
}

func TestPlanCapsFrom_ReadsThePlansBlock(t *testing.T) {
	doc := policy.StaffedProfile()
	doc.Plans = policy.PlanCaps{MaxNewIndexesPerTable: 4,
		PeakWindows: []string{"weekdays 09:00-17:00 UTC"}}
	reads := &fakePolicyReads{policy: storedPolicy(t, doc)}
	caps, err := planCapsFrom(reads.read)(context.Background())
	got := caps.Resolved()
	if err != nil || got.MaxNewIndexesPerTable != 4 || got.MaxNewIndexBytesPct != 10 ||
		len(got.PeakWindows) != 1 || reads.calls != 1 {
		t.Fatalf("caps %+v (%v, %d reads), want 4, 10 and one peak window", got, err,
			reads.calls)
	}
}

// Nil/empty: a stored policy without plans gives the defaults.
func TestPlanCapsFrom_NoPlansBlockGivesTheDefaults(t *testing.T) {
	reads := &fakePolicyReads{policy: storedPolicy(t, policy.UnattendedProfile())}
	caps, err := planCapsFrom(reads.read)(context.Background())
	got := caps.Resolved()
	if err != nil || got.MaxNewIndexesPerTable != 2 || got.MaxNewIndexBytesPct != 10 ||
		got.MaxRestartBoundSteps != 1 || len(got.PeakWindows) != 0 {
		t.Fatalf("caps %+v (%v), want the defaults", got, err)
	}
}

// Error propagation: a failed read and an unparsable document are errors
// (the engine then waits), never the defaults.
func TestPlanCapsFrom_FailuresAreErrors(t *testing.T) {
	reads := &fakePolicyReads{err: errors.New("control database unreachable")}
	if _, err := planCapsFrom(reads.read)(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "control database unreachable") {
		t.Fatalf("failed read: %v, want the read's error", err)
	}
	bad := storedPolicy(t, policy.StaffedProfile())
	bad.Document = json.RawMessage(strings.Replace(string(bad.Document), `"serialize_mode"`,
		`"plans":{"max_new_index_bytes_pct":51},"serialize_mode"`, 1))
	reads = &fakePolicyReads{policy: bad}
	if _, err := planCapsFrom(reads.read)(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "plans.max_new_index_bytes_pct") {
		t.Fatalf("invalid document: %v, want the plans refusal", err)
	}
}
