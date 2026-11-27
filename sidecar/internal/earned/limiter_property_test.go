package earned

import (
	"context"
	"errors"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"
)

// earned-08: the gate (Limit), the Autonomy page (Annotate) and the Trust
// page (AnnotateTrust) show and enforce one effective-level rule. These
// tests need no database: the views carry the safety record they read,
// and the downgrade sources are fakes.

var propertyNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// countingHA is an HA source that counts its reads.
type countingHA struct {
	mu    sync.Mutex
	state HAState
	calls int
}

func (h *countingHA) HAStatus(context.Context) (HAState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.state, nil
}

func (h *countingHA) reads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (b *fakeBudget) reads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// pureLimiter is a limiter on a service without a store: anything that
// would read the ledger database panics, so a view that does not carry
// its safety record fails the test.
func pureLimiter(budget *fakeBudget, ha *countingHA) *Limiter {
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return propertyNow }
	svc := &Service{cfg: cfg, cache: map[pairKey]cachedLevel{}}
	return svc.Limiter(Binding{Database: "orders", Budget: budget, HA: ha})
}

// ledgerWorld is one random ledger: stored rows (carried-over,
// grandfathered and earned, some pairs left at their default), the level
// each pair's evidence supports, the families' safety record and the
// database-wide signals.
type ledgerWorld struct {
	levels    []State
	supported map[pairKey]Level
	safety    map[Family]familySafetyRow
	budget    BudgetState
	budgetErr error
	replica   bool
}

func randomWorld(r *rand.Rand) ledgerWorld {
	w := ledgerWorld{supported: map[pairKey]Level{}, safety: map[Family]familySafetyRow{},
		budget: BudgetState{Configured: true}}
	provenances := []string{ProvenanceLedger, ProvenanceCarriedOver, ProvenanceGrandfathered}
	for _, key := range gridPairs() {
		if r.Intn(4) > 0 {
			w.levels = append(w.levels, State{Family: key.family, Class: key.class,
				Level: Level(r.Intn(int(MaxGrantable) + 1)), Stored: true,
				Provenance: provenances[r.Intn(len(provenances))]})
		}
		w.supported[key] = Level(r.Intn(int(L3) + 1))
	}
	for _, f := range Families() {
		w.safety[f] = familySafetyRow{}
		if r.Intn(3) == 0 {
			w.safety[f] = familySafetyRow{n: 1 + r.Intn(3), last: propertyNow.Add(-time.Hour)}
		}
	}
	switch r.Intn(3) {
	case 1:
		w.budget = BudgetState{Configured: true, FastBurning: true, Detail: "fast burn"}
	case 2:
		w.budgetErr = errors.New("slo store unreachable")
	}
	w.replica = r.Intn(4) == 0
	return w
}

func (w ledgerWorld) limiter() (*Limiter, *fakeBudget, *countingHA) {
	b := &fakeBudget{state: w.budget, err: w.budgetErr}
	ha := &countingHA{state: HAState{Role: RolePrimary}}
	if w.replica {
		ha.state.Role = "replica"
	}
	return pureLimiter(b, ha), b, ha
}

// granted is a pair's level as Granted reads it (composite rows bounded by
// their components).
func (w ledgerWorld) granted(key pairKey) State {
	in := &ledgerReads{levels: w.levels}
	return in.granted(key.family, key.class)
}

// wantReasons are the downgrade reasons a pair of f shows.
func (w ledgerWorld) wantReasons(f Family) []string {
	var out []string
	switch {
	case w.budgetErr != nil:
		out = append(out, DowngradeBudgetUnavailable)
	case w.budget.FastBurning:
		out = append(out, DowngradeBudgetBurn)
	}
	if w.replica {
		out = append(out, DowngradeHARole)
	}
	if !IsSelfInitiated(f) && w.safety[f].n > 0 {
		out = append(out, DowngradeSafetyRegression)
	}
	return out
}

// gate is the level Limit's rule allows the pair under contract, with the
// database-wide signals of the world.
func (w ledgerWorld) gate(t *testing.T, key pairKey, contract Level) Level {
	t.Helper()
	var downs []Downgrade
	for _, reason := range w.wantReasons(key.family) {
		downs = append(downs, Downgrade{Reason: reason})
	}
	level, err := effectiveLevel(w.granted(key), contract, downs,
		func() (Level, error) { return w.supported[key], nil })
	if err != nil {
		t.Fatalf("effectiveLevel(%v): %v", key, err)
	}
	return level
}

func (w ledgerWorld) view() View {
	v := View{safety: w.safety}
	for _, f := range Families() {
		fv := FamilyView{Family: f}
		for _, c := range ApplicableClasses(f) {
			st := w.granted(pairKey{f, c})
			fv.Classes = append(fv.Classes, ClassView{Class: c, Cap: CapForPair(f, c),
				Granted: st.Level, Supported: w.supported[pairKey{f, c}],
				Provenance: st.Provenance})
		}
		v.Families = append(v.Families, fv)
	}
	return v
}

func (w ledgerWorld) trustView() TrustView {
	v := TrustView{safety: w.safety}
	for _, key := range gridPairs() {
		st := w.granted(key)
		row := TrustRow{Family: key.family, Class: key.class, Level: st.Level,
			Cap: CapForPair(key.family, key.class), Provenance: st.Provenance}
		if !IsSelfInitiated(key.family) {
			sup := w.supported[key]
			row.Supported = &sup
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

func reasonsOf(downs []Downgrade) []string {
	var out []string
	for _, d := range downs {
		out = append(out, d.Reason)
	}
	return out
}

// Seeded random ledgers over every grid pair: the Autonomy page and the
// Trust page show exactly the level the gate allows under a reversible
// contract, with the same downgrades, and no contract lets the gate allow
// more than the pages show.
func TestEffectiveLevelAgreesAcrossGateAndViews(t *testing.T) {
	if n := len(gridPairs()); n < 36 {
		t.Fatalf("grid has %d pairs, want every family x class (>= 36)", n)
	}
	for seed := int64(1); seed <= 300; seed++ {
		w := randomWorld(rand.New(rand.NewSource(seed)))
		lim, _, _ := w.limiter()
		v, tv := w.view(), w.trustView()
		lim.Annotate(context.Background(), &v)
		lim.AnnotateTrust(context.Background(), &tv)
		for _, fam := range v.Families {
			for _, row := range fam.Classes {
				key := pairKey{fam.Family, row.Class}
				checkRow(t, seed, "autonomy", w, key, row.Effective, row.Downgrades)
			}
		}
		for _, row := range tv.Rows {
			key := pairKey{row.Family, row.Class}
			checkRow(t, seed, "trust", w, key, row.Effective, row.Downgrades)
		}
		for _, key := range gridPairs() {
			best := w.gate(t, key, L3)
			for _, contract := range []Level{L0, L1, L2, L3} {
				// The evidence binds only from L2, so a contract below L2 can
				// allow more than L3 does when the evidence supports L0. No
				// applicable pair's evidence supports L0 (its L1 checks are the
				// family and the class), so the pages' best case holds there.
				got := w.gate(t, key, contract)
				if got > contract || (got > best && w.supported[key] >= L1) {
					t.Fatalf("seed %d %v: contract %v allows %v, above %v or the contract",
						seed, key, contract, got, best)
				}
			}
		}
		if t.Failed() {
			return
		}
	}
}

func checkRow(t *testing.T, seed int64, page string, w ledgerWorld, key pairKey,
	effective *Level, downs []Downgrade) {
	t.Helper()
	want := w.gate(t, key, L3)
	if effective == nil || *effective != want {
		t.Errorf("seed %d %s page %v: effective %v, gate allows %v (state %+v)", seed,
			page, key, effective, want, w.granted(key))
	}
	if got, wantR := reasonsOf(downs), w.wantReasons(key.family); !slices.Equal(got,
		wantR) {
		t.Errorf("seed %d %s page %v: downgrades %v, want %v", seed, page, key, got, wantR)
	}
}

// Each annotation reads the error budget and the HA role once, however
// many families the view shows.
func TestEffectiveLevelAnnotateReadsSignalsOncePerCall(t *testing.T) {
	w := randomWorld(rand.New(rand.NewSource(7)))
	w.budget, w.budgetErr = BudgetState{Configured: true}, nil
	lim, budget, ha := w.limiter()
	v := w.view()
	lim.Annotate(context.Background(), &v)
	if len(v.Families) < 2 || budget.reads() != 1 || ha.reads() != 1 {
		t.Fatalf("Annotate over %d families read the budget %d and HA %d times, want 1",
			len(v.Families), budget.reads(), ha.reads())
	}
	tv := w.trustView()
	lim.AnnotateTrust(context.Background(), &tv)
	if budget.reads() != 2 || ha.reads() != 2 {
		t.Fatalf("AnnotateTrust read the budget %d and HA %d times in all, want 2",
			budget.reads(), ha.reads())
	}
}

// An empty view is annotated without reading anything and without panic.
func TestEffectiveLevelEmptyViews(t *testing.T) {
	lim, budget, ha := (ledgerWorld{budget: BudgetState{Configured: true}}).limiter()
	v := View{}
	lim.Annotate(context.Background(), &v)
	if len(v.Families) != 0 || budget.reads() != 0 || ha.reads() != 0 {
		t.Fatalf("empty view: %d families, budget %d, HA %d reads", len(v.Families),
			budget.reads(), ha.reads())
	}
	tv := TrustView{safety: map[Family]familySafetyRow{}}
	lim.AnnotateTrust(context.Background(), &tv)
	if len(tv.Rows) != 0 {
		t.Fatalf("empty trust view grew %d rows", len(tv.Rows))
	}
}

// Concurrent annotations through one limiter agree with a sequential one.
func TestEffectiveLevelConcurrentAnnotate(t *testing.T) {
	w := randomWorld(rand.New(rand.NewSource(11)))
	lim, _, _ := w.limiter()
	want := w.view()
	lim.Annotate(context.Background(), &want)
	var wg sync.WaitGroup
	got := make([]View, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = w.view()
			lim.Annotate(context.Background(), &got[i])
		}(i)
	}
	wg.Wait()
	for i := range got {
		for f := range want.Families {
			for c, row := range want.Families[f].Classes {
				g := got[i].Families[f].Classes[c]
				if g.Effective == nil || *g.Effective != *row.Effective {
					t.Fatalf("goroutine %d %s/%s: effective %v, sequential %v", i,
						want.Families[f].Family, row.Class, g.Effective, *row.Effective)
				}
			}
		}
	}
}
