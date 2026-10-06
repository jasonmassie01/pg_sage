package earned

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// index_replace (roadmap 2.3) creates a wider index and soft-drops the one
// it subsumes in one approved action. Product call: it is reversible (the
// old index is re-created from its definition) but capped at L2, the
// one-click handoff, for every family including the self-initiated
// tuning family, and its level never exceeds the levels of its two
// components (index_create in tuning, index_drop in hygiene).

// No concurrent access tests: the class table is immutable package data.

func TestIndexReplaceClass(t *testing.T) {
	req := selfReq("replace_index", "CREATE INDEX CONCURRENTLY i ON public.orders (a, b)")
	if got := ClassFor(req); got != ClassIndexReplace {
		t.Fatalf("ClassFor = %q", got)
	}
	if got := ClassForActionType("replace_index"); got != ClassIndexReplace {
		t.Fatalf("ClassForActionType = %q", got)
	}
	spec, ok := Spec(ClassIndexReplace)
	if !ok || spec.Reversibility != Reversible || spec.Cap != L2 {
		t.Fatalf("spec = %+v", spec)
	}
	if CapFor(ClassIndexReplace) != L2 || CapForPair(FamilyTuning, ClassIndexReplace) != L2 {
		t.Fatalf("caps = %v / %v, want L2", CapFor(ClassIndexReplace),
			CapForPair(FamilyTuning, ClassIndexReplace))
	}
	if CapForPair(FamilyHygiene, ClassIndexReplace) != L1 {
		t.Fatal("a pair outside the class's family never rises above L1")
	}
	if !Applicable(FamilyTuning, ClassIndexReplace) ||
		SelfFamilyFor(ClassIndexReplace) != FamilyTuning {
		t.Fatal("index_replace is a self-initiated tuning class")
	}
	if OutcomeClassFor(ClassIndexReplace) != verify.ClassIndexReplace ||
		ClassForOutcomeClass(verify.ClassIndexReplace) != ClassIndexReplace {
		t.Fatal("its evidence is recorded under the index_replace verification class")
	}
	if classForActionLabel("replace_index") != ClassIndexReplace {
		t.Fatal("action_log label replace_index maps to the class")
	}
	comps := ComponentClasses(ClassIndexReplace)
	if len(comps) != 2 || comps[0] != (Pair{FamilyTuning, ClassIndexCreate}) ||
		comps[1] != (Pair{FamilyHygiene, ClassIndexDrop}) {
		t.Fatalf("components = %+v", comps)
	}
	if len(ComponentClasses(ClassIndexCreate)) != 0 {
		t.Fatal("a plain class has no components")
	}
}

// The cap on the self-initiated pair keeps the special case narrow: the
// other reversible self-initiated classes still reach L3 (GUCs included).
func TestIndexReplaceCapDoesNotLowerOtherClasses(t *testing.T) {
	for _, c := range []ActionClass{ClassIndexCreate, ClassConfigGUC, ClassQueryHint,
		ClassStatistics, ClassAutovacuumTuning} {
		if got := CapForPair(FamilyTuning, c); got != L3 {
			t.Fatalf("CapForPair(tuning, %s) = %v, want L3", c, got)
		}
	}
}

func TestIndexReplaceGrandfatheredAtMostL2(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	auto := grandfatherBounds(now)["auto"]
	if got, _ := GrandfatheredLevel(auto, now, ClassIndexReplace); got != L2 {
		t.Fatalf("an autonomous ramp grants index_replace %v, want L2", got)
	}
	approval := grandfatherBounds(now)["approval"]
	if got, _ := GrandfatheredLevel(approval, now, ClassIndexReplace); got != L2 {
		t.Fatalf("approval mode grants index_replace %v, want L2", got)
	}
	if got, _ := GrandfatheredLevel(policy.RuntimeState{}, now, ClassIndexReplace); got != L1 {
		t.Fatalf("a disabled executor grants %v", got)
	}
}
