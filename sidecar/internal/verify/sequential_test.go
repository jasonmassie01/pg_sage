package verify

import (
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Repeated looks (lab-01b): the verifier looks at a watch at 2, 4, 8, 16,
// 32, 64 and 72 h with the defaults. Each look spends part of alpha by an
// O'Brien-Fleming-type Lan-DeMets function, so the looks together keep the
// false-positive rate at alpha. Pure functions: no shared state, so no
// concurrent-access test applies beyond the package's -race run.

var defaultLooks = []time.Duration{2 * time.Hour, 4 * time.Hour, 8 * time.Hour,
	16 * time.Hour, 32 * time.Hour, 64 * time.Hour, 72 * time.Hour}

func TestSequentialLookScheduleFollowsTheDoubling(t *testing.T) {
	d := DefaultOptions()
	if got := LookSchedule(d.InitialWindow, d.HardMax); !slices.Equal(got, defaultLooks) {
		t.Fatalf("default schedule %v, want %v", got, defaultLooks)
	}
	for _, tc := range []struct {
		initial, max time.Duration
		want         []time.Duration
	}{
		{3 * time.Hour, 12 * time.Hour, []time.Duration{3 * time.Hour, 6 * time.Hour,
			12 * time.Hour}},
		{72 * time.Hour, 72 * time.Hour, []time.Duration{72 * time.Hour}},
		{80 * time.Hour, 72 * time.Hour, []time.Duration{80 * time.Hour}},
		{0, 72 * time.Hour, nil},
		{2 * time.Hour, 0, nil},
		{-time.Hour, 72 * time.Hour, nil},
	} {
		if got := LookSchedule(tc.initial, tc.max); !slices.Equal(got, tc.want) {
			t.Errorf("LookSchedule(%s, %s) = %v, want %v", tc.initial, tc.max, got, tc.want)
		}
	}
}

// The schedule matches insufficientVerdict's doubling look for look.
func TestSequentialLookScheduleMatchesTheEngineDoubling(t *testing.T) {
	e := &Engine{options: DefaultOptions()}
	state := WatchState{Window: 2 * time.Hour, Criterion: Criterion{HardMax: 72 * time.Hour}}
	var seen []time.Duration
	for range 10 {
		seen = append(seen, state.Window)
		v := e.insufficientVerdict(state, 0)
		if v.Status != "extended" {
			break
		}
		state.Window = v.Window
	}
	if !slices.Equal(seen, defaultLooks) {
		t.Fatalf("engine looks %v, schedule %v", seen, defaultLooks)
	}
}

func TestSequentialLookIndex(t *testing.T) {
	for _, tc := range []struct {
		window time.Duration
		index  int
		final  bool
	}{{2 * time.Hour, 1, false}, {3 * time.Hour, 2, false}, {64 * time.Hour, 6, false},
		{72 * time.Hour, 7, true}, {100 * time.Hour, 7, true}, {time.Minute, 1, false}} {
		i, final, err := LookIndex(defaultLooks, tc.window)
		if err != nil || i != tc.index || final != tc.final {
			t.Errorf("LookIndex(%s) = %d %v %v, want %d %v", tc.window, i, final, err,
				tc.index, tc.final)
		}
	}
	if _, _, err := LookIndex(nil, time.Hour); !errors.Is(err, ErrNoLooks) {
		t.Fatalf("zero looks: %v, want ErrNoLooks", err)
	}
}

func TestSequentialCumulativeAlpha(t *testing.T) {
	if got := CumulativeAlpha(0.05, 1); math.Abs(got-0.05) > 1e-12 {
		t.Fatalf("alpha(1) = %v, want 0.05", got)
	}
	if got := CumulativeAlpha(0.05, 0); got != 0 {
		t.Fatalf("alpha(0) = %v, want 0", got)
	}
	// 2-2*Phi(1.959964/sqrt(0.5)) = 2-2*Phi(2.771808) = 0.005575
	if got := CumulativeAlpha(0.05, 0.5); math.Abs(got-0.005575) > 2e-6 {
		t.Fatalf("alpha(0.5) = %v, want 0.005575", got)
	}
	prev := 0.0
	for f := 0.05; f <= 1.0001; f += 0.05 {
		got := CumulativeAlpha(0.05, f)
		if got < prev {
			t.Fatalf("alpha(%v) = %v fell below %v", f, got, prev)
		}
		prev = got
	}
	if got := CumulativeAlpha(0.05, 2); math.Abs(got-0.05) > 1e-12 {
		t.Fatalf("alpha(2) = %v, want capped at 0.05", got)
	}
}

// spendAll runs the looks of schedule from prev with time fractions.
func spendAll(t *testing.T, prev AlphaSpend, schedule []time.Duration, from int) AlphaSpend {
	t.Helper()
	hardMax := schedule[len(schedule)-1]
	for i := from; i < len(schedule); i++ {
		f := InformationFraction(0, 0, schedule[i], hardMax)
		look, next, err := SpendLook(prev, DefaultAlpha, f, i == len(schedule)-1)
		if err != nil {
			t.Fatalf("look %d: %v", i+1, err)
		}
		if look.Index != i+1 || math.Abs(look.Spent-next.Spent) > 0 {
			t.Fatalf("look %d: %+v next %+v", i+1, look, next)
		}
		prev = next
	}
	return prev
}

// Boundary: the last look spends exactly the remaining alpha: the per-look
// alphas sum to 0.05 within 1e-9.
func TestSequentialSpendLookLastLookSpendsTheRemainder(t *testing.T) {
	s := spendAll(t, AlphaSpend{}, defaultLooks, 0)
	if s.Looks != 7 || len(s.PerLook) != 7 || s.Fraction != 1 {
		t.Fatalf("after seven looks: %+v", s)
	}
	var sum float64
	for _, a := range s.PerLook {
		if a < 0 {
			t.Fatalf("negative look alpha in %v", s.PerLook)
		}
		sum += a
	}
	if math.Abs(sum-DefaultAlpha) > 1e-9 || math.Abs(s.Spent-DefaultAlpha) > 1e-9 {
		t.Fatalf("per-look alphas sum to %v (spent %v), want 0.05", sum, s.Spent)
	}
	before := s.Spent - s.PerLook[6]
	if math.Abs(s.PerLook[6]-(DefaultAlpha-before)) > 1e-15 {
		t.Fatalf("last look %v is not the remainder %v", s.PerLook[6], DefaultAlpha-before)
	}
	// O'Brien-Fleming: the early looks spend almost nothing.
	if s.PerLook[0] > 1e-12 || s.PerLook[1] > 1e-12 || !(s.PerLook[5] > s.PerLook[3]) {
		t.Fatalf("spending is not O'Brien-Fleming shaped: %v", s.PerLook)
	}
}

// A look that adds no information spends nothing; a final look after
// alpha is spent spends nothing more.
func TestSequentialSpendLookNeverGoesBack(t *testing.T) {
	_, s, err := SpendLook(AlphaSpend{}, DefaultAlpha, 0.6, false)
	if err != nil {
		t.Fatal(err)
	}
	look, next, err := SpendLook(s, DefaultAlpha, 0.4, false)
	if err != nil || look.Alpha != 0 || next.Fraction != 0.6 || next.Spent != s.Spent {
		t.Fatalf("a look with less information: %+v %+v %v", look, next, err)
	}
	_, done, _ := SpendLook(next, DefaultAlpha, 1, true)
	look, again, err := SpendLook(done, DefaultAlpha, 1, true)
	if err != nil || look.Alpha != 0 || again.Spent != done.Spent {
		t.Fatalf("a look after the final one: %+v %+v %v", look, again, err)
	}
}

// Invalid input: alpha outside (0,1), a NaN or negative fraction and a
// corrupt stored spend are errors; never a fresh alpha.
func TestSequentialSpendLookRefusesBadInput(t *testing.T) {
	for name, tc := range map[string]struct {
		prev            AlphaSpend
		alpha, fraction float64
	}{
		"alpha 0":        {AlphaSpend{}, 0, 0.5},
		"alpha 1":        {AlphaSpend{}, 1, 0.5},
		"alpha NaN":      {AlphaSpend{}, math.NaN(), 1},
		"fraction NaN":   {AlphaSpend{}, 0.05, math.NaN()},
		"fraction < 0":   {AlphaSpend{}, 0.05, -0.1},
		"overspent":      {AlphaSpend{Looks: 3, Spent: 0.06}, 0.05, 0.5},
		"negative spend": {AlphaSpend{Looks: 1, Spent: -0.01}, 0.05, 0.5},
		"NaN spend":      {AlphaSpend{Looks: 1, Spent: math.NaN()}, 0.05, 0.5},
	} {
		look, next, err := SpendLook(tc.prev, tc.alpha, tc.fraction, false)
		if err == nil || look.Alpha != 0 || !reflect.DeepEqual(next, AlphaSpend{}) {
			t.Errorf("%s: %+v %+v %v, want an error and nothing spent", name, look, next, err)
		}
	}
}

func TestSequentialInformationFraction(t *testing.T) {
	for _, tc := range []struct {
		calls, expected float64
		window          time.Duration
		want            float64
	}{
		{500, 1000, time.Hour, 0.5},
		{1500, 1000, time.Hour, 1},
		{0, 1000, time.Hour, 0},
		{500, 0, 18 * time.Hour, 0.25},
		{500, math.NaN(), 36 * time.Hour, 0.5},
		{math.Inf(1), 1000, 36 * time.Hour, 0.5},
		{-5, 1000, 36 * time.Hour, 0.5},
		{0, 0, 0, 0},
	} {
		got := InformationFraction(tc.calls, tc.expected, tc.window, 72*time.Hour)
		if math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("InformationFraction(%v, %v, %s) = %v, want %v", tc.calls, tc.expected,
				tc.window, got, tc.want)
		}
	}
	if got := InformationFraction(0, 0, time.Hour, 0); got != 1 {
		t.Fatalf("no hard max: %v, want 1 (one look)", got)
	}
}

// Bonferroni across targets stays on top of the look's alpha.
func TestSequentialTargetAlpha(t *testing.T) {
	if got := TargetAlpha(0.01, 4); got != 0.0025 {
		t.Fatalf("TargetAlpha(0.01, 4) = %v", got)
	}
	if TargetAlpha(0.01, 1) != 0.01 || TargetAlpha(0.01, 0) != 0.01 {
		t.Fatal("one or no target must keep the look's alpha")
	}
}

// State transition: the spend survives a sidecar restart between look 3
// and 4 through the watch baseline JSON, and the run ends where the
// uninterrupted one does. Recomputing from zero would not.
func TestSequentialAlphaSpendSurvivesARestart(t *testing.T) {
	whole := spendAll(t, AlphaSpend{}, defaultLooks, 0)
	three := AlphaSpend{}
	for i := 0; i < 3; i++ {
		f := InformationFraction(0, 0, defaultLooks[i], 72*time.Hour)
		_, three, _ = SpendLook(three, DefaultAlpha, f, false)
	}
	state := WatchState{ID: "w1", ExecutedAt: cusumStep, AlphaSpend: three}
	criterion, baseline, err := marshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(baseline), `"alpha_spend":{"looks":3,`) {
		t.Fatalf("baseline %s lacks the spend", baseline)
	}
	restored, err := (&PostgresStateStore{}).scanState(fakeScanner{criterion, baseline})
	if err != nil || !reflect.DeepEqual(restored.AlphaSpend, three) {
		t.Fatalf("restored %+v (%v), want %+v", restored.AlphaSpend, err, three)
	}
	resumed := spendAll(t, restored.AlphaSpend, defaultLooks, 3)
	if !reflect.DeepEqual(resumed, whole) {
		t.Fatalf("resumed %+v, uninterrupted %+v", resumed, whole)
	}
	f4 := InformationFraction(0, 0, defaultLooks[3], 72*time.Hour)
	fresh, _, _ := SpendLook(AlphaSpend{}, DefaultAlpha, f4, false)
	if fresh.Alpha == whole.PerLook[3] {
		t.Fatal("a restart from zero spends the same: the test cannot tell them apart")
	}
	_, plain, _ := marshalState(WatchState{ID: "w0"})
	if strings.Contains(string(plain), "alpha_spend") {
		t.Fatalf("a watch without looks stores a spend: %s", plain)
	}
}

// zCrit is the two-sided critical value of the standard normal at alpha.
func zCrit(alpha float64) float64 { return math.Sqrt2 * math.Erfinv(1-alpha) }

// nullRejects simulates one null series of hourly N(0,1) residuals judged
// at each look by a z-test of the cumulative mean; alphaOf gives look i's
// alpha (0-based). It reports whether any look rejected.
func nullRejects(r *rand.Rand, alphaOf func(i int) float64) bool {
	hours := int(defaultLooks[len(defaultLooks)-1] / time.Hour)
	var sum float64
	n := 0
	rejected := false
	for i, look := range defaultLooks {
		for ; n < int(look/time.Hour) && n < hours; n++ {
			sum += r.NormFloat64()
		}
		if a := alphaOf(i); a > 0 && math.Abs(sum/math.Sqrt(float64(n))) > zCrit(a) {
			rejected = true
		}
	}
	return rejected
}

// Simulation (deterministic seed): 2,000 null series with seven looks. v2
// keeps false positives at or below 5.5%; independent 0.05 looks exceed
// 12% (the bug this fixes).
func TestSequentialSimulationKeepsTheFalsePositiveRate(t *testing.T) {
	spend := spendAll(t, AlphaSpend{}, defaultLooks, 0)
	const runs = 2000
	r1 := rand.New(rand.NewPCG(2026, 11))
	r2 := rand.New(rand.NewPCG(2026, 11))
	v2, naive := 0, 0
	for range runs {
		if nullRejects(r1, func(i int) float64 { return spend.PerLook[i] }) {
			v2++
		}
		if nullRejects(r2, func(int) float64 { return DefaultAlpha }) {
			naive++
		}
	}
	v2Rate, naiveRate := float64(v2)/runs, float64(naive)/runs
	t.Logf("null false-positive rate over %d series, 7 looks: v2 %.2f%%, "+
		"independent 0.05 looks %.2f%%", runs, 100*v2Rate, 100*naiveRate)
	if v2Rate > 0.055 {
		t.Fatalf("v2 false-positive rate %.2f%%, want <= 5.5%%", 100*v2Rate)
	}
	if naiveRate <= 0.12 {
		t.Fatalf("independent looks %.2f%%: the pinned bug is not shown (want > 12%%)",
			100*naiveRate)
	}
}

// Boundary: the last look (at HardMax) spends what is left even when the
// calls seen fall short of the calls expected, so its information fraction
// is below 1; otherwise a watch on a quiet target would leave alpha unspent.
func TestSequentialFinalLookAtPartialInformationSpendsTheRest(t *testing.T) {
	_, prev, err := SpendLook(AlphaSpend{}, DefaultAlpha, 0.3, false)
	if err != nil {
		t.Fatal(err)
	}
	f := InformationFraction(400, 1000, 72*time.Hour, 72*time.Hour)
	look, next, err := SpendLook(prev, DefaultAlpha, f, true)
	if err != nil || f != 0.4 {
		t.Fatalf("fraction %v: %v", f, err)
	}
	if !look.Final || next.Fraction != 1 || math.Abs(next.Spent-DefaultAlpha) > 1e-15 ||
		math.Abs(look.Alpha-(DefaultAlpha-prev.Spent)) > 1e-15 {
		t.Fatalf("final look at fraction 0.4: %+v %+v, want the remaining %v", look, next,
			DefaultAlpha-prev.Spent)
	}
	if CumulativeAlpha(DefaultAlpha, 0.4) >= DefaultAlpha/2 {
		t.Fatal("the spending function alone would spend the rest: the test cannot tell")
	}
}
