package earned

import (
	"context"
	"errors"
	"testing"
)

// earned-08 boundaries of the one effective-level rule, each held on the
// gate's rule and, where a page can show the pair, on both pages.

// pairWhere is the first grid pair that keep accepts.
func pairWhere(t *testing.T, what string, keep func(pairKey) bool) pairKey {
	t.Helper()
	for _, key := range gridPairs() {
		if keep(key) {
			return key
		}
	}
	t.Fatalf("no grid pair is %s", what)
	return pairKey{}
}

func irreversibleIncidentPair(t *testing.T) pairKey {
	return pairWhere(t, "an irreversible incident pair", func(k pairKey) bool {
		spec, ok := Spec(k.class)
		return ok && !IsSelfInitiated(k.family) && spec.Reversibility == Irreversible
	})
}

func selfL3Pair(t *testing.T) pairKey {
	return pairWhere(t, "a self-initiated pair capped at L3", func(k pairKey) bool {
		return IsSelfInitiated(k.family) && CapForPair(k.family, k.class) == L3 &&
			len(ComponentClasses(k.class)) == 0
	})
}

// nonApplicablePair is a shipped incident family with an L3 class that
// does not remediate it.
func nonApplicablePair(t *testing.T) pairKey {
	t.Helper()
	for _, s := range classSpecs {
		if s.Cap == L3 && !Applicable(FamilyWraparound, s.Class) {
			return pairKey{FamilyWraparound, s.Class}
		}
	}
	t.Fatal("every L3 class remediates wraparound")
	return pairKey{}
}

type boundaryCase struct {
	name       string
	key        pairKey
	level      Level
	provenance string
	supported  Level
	contract   Level
	downgraded bool
	// readsSupport says whether the rule may consult the evidence.
	readsSupport bool
	// onPages says whether the pages show the pair (applicable pairs).
	onPages bool
	want    Level
}

func boundaryCases(t *testing.T) []boundaryCase {
	freeze := pairKey{FamilyWraparound, ClassFreeze}
	irrev, self := irreversibleIncidentPair(t), selfL3Pair(t)
	return []boundaryCase{
		{"granted L1 with supported L0", freeze, L1, ProvenanceLedger, L0, L3, false,
			false, true, L1},
		{"carried-over irreversible", irrev, L3, ProvenanceCarriedOver, L3, L3, false,
			false, true, L1},
		{"carried-over reversible keeps its level", freeze, L3, ProvenanceCarriedOver, L0,
			L3, false, false, true, L3},
		{"non-applicable pair", nonApplicablePair(t), L3, ProvenanceLedger, L3, L3, false,
			false, false, L1},
		{"earned L3 decays to its evidence", freeze, L3, ProvenanceLedger, L2, L3, false,
			true, true, L2},
		{"grandfathered decays", freeze, L3, ProvenanceGrandfathered, L1, L3, false, true,
			true, L1},
		{"granted L2 above an L1 cap", irrev, L2, ProvenanceLedger, L0, L3, false, false,
			true, L1},
		{"irreversible contract", freeze, L3, ProvenanceLedger, L3, L1, false, false, false,
			L1},
		{"mitigation-only contract", freeze, L3, ProvenanceLedger, L3, L2, false, true,
			false, L2},
		{"downgrade caps at L1", freeze, L3, ProvenanceLedger, L3, L3, true, true, true, L1},
		{"downgrade never raises L0", freeze, L0, ProvenanceLedger, L3, L3, true, false,
			true, L0},
		{"self-initiated ignores incident evidence", self, L3, ProvenanceLedger, L0, L3,
			false, false, true, L3},
	}
}

func TestEffectiveLevelBoundaries(t *testing.T) {
	for _, tc := range boundaryCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			st := State{Family: tc.key.family, Class: tc.key.class, Level: tc.level,
				Provenance: tc.provenance}
			var downs []Downgrade
			if tc.downgraded {
				downs = []Downgrade{{Reason: DowngradeBudgetBurn}}
			}
			reads := 0
			got, err := effectiveLevel(st, tc.contract, downs, func() (Level, error) {
				reads++
				return tc.supported, nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("gate level = %v (%v), want %v", got, err, tc.want)
			}
			if !tc.readsSupport && reads != 0 {
				t.Fatalf("the evidence was read %d times where it cannot lower the level",
					reads)
			}
			if tc.onPages {
				checkPages(t, tc)
			}
		})
	}
}

// checkPages shows the case's pair alone on both pages.
func checkPages(t *testing.T, tc boundaryCase) {
	t.Helper()
	w := ledgerWorld{supported: map[pairKey]Level{tc.key: tc.supported},
		safety: map[Family]familySafetyRow{}, budget: BudgetState{Configured: true},
		levels: []State{{Family: tc.key.family, Class: tc.key.class, Level: tc.level,
			Provenance: tc.provenance, Stored: true}}}
	if tc.downgraded {
		w.budget.FastBurning = true
	}
	lim, _, _ := w.limiter()
	v := View{safety: w.safety, Families: []FamilyView{{Family: tc.key.family,
		Classes: []ClassView{{Class: tc.key.class, Cap: CapForPair(tc.key.family,
			tc.key.class), Granted: tc.level, Supported: tc.supported,
			Provenance: tc.provenance}}}}}
	lim.Annotate(context.Background(), &v)
	if e := v.Families[0].Classes[0].Effective; e == nil || *e != tc.want {
		t.Errorf("autonomy page shows %v, want %v", e, tc.want)
	}
	tv := w.trustView()
	lim.AnnotateTrust(context.Background(), &tv)
	for _, row := range tv.Rows {
		if row.Family == tc.key.family && row.Class == tc.key.class &&
			(row.Effective == nil || *row.Effective != tc.want) {
			t.Errorf("trust page shows %v, want %v", row.Effective, tc.want)
		}
	}
}

// A failed evidence read is the gate's error (it then fails closed), never
// a level.
func TestEffectiveLevelSupportReadErrorIsReturned(t *testing.T) {
	boom := errors.New("evidence read failed")
	st := State{Family: FamilyWraparound, Class: ClassFreeze, Level: L3,
		Provenance: ProvenanceLedger}
	got, err := effectiveLevel(st, L3, nil, func() (Level, error) { return L3, boom })
	if !errors.Is(err, boom) || got != L0 {
		t.Fatalf("effectiveLevel = %v, %v; want L0 and the read error", got, err)
	}
}

// A zero state (no family, no class) is not applicable: at most L1, and
// L0 by default.
func TestEffectiveLevelZeroState(t *testing.T) {
	got, err := effectiveLevel(State{}, L3, nil, func() (Level, error) {
		t.Fatal("a zero state's evidence was read")
		return L0, nil
	})
	if err != nil || got != L0 {
		t.Fatalf("zero state = %v (%v), want L0", got, err)
	}
	got, err = effectiveLevel(State{Level: L3}, L3, nil, func() (Level, error) {
		return L3, nil
	})
	if err != nil || got != L1 {
		t.Fatalf("unknown pair at L3 = %v (%v), want at most L1", got, err)
	}
}
