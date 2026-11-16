package backtest

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// The gate lab-01c's auto reads: v2, 200 usable placebos, a rate at or
// below 5%, detection at or above 80%. Boundaries compare exactly.

func passing() Counts {
	return Counts{Placebos: 300, Excluded: 0, FalsePositives: 3, Inconclusive: 10,
		Injected: 300, Detected: 290}
}

func TestGate_Boundaries(t *testing.T) {
	cases := []struct {
		name   string
		c      Counts
		ok     bool
		reason string
	}{
		{"passing", passing(), true, ""},
		{"199 usable", Counts{Placebos: 210, Excluded: 11, Injected: 199, Detected: 199},
			false, "199 usable placebos below 200"},
		{"200 usable", Counts{Placebos: 210, Excluded: 10, Injected: 200, Detected: 200},
			true, ""},
		{"rate exactly 5%", Counts{Placebos: 200, FalsePositives: 10, Injected: 200,
			Detected: 200}, true, ""},
		{"rate 5.01%", Counts{Placebos: 10000, FalsePositives: 501, Injected: 10000,
			Detected: 10000}, false, "false-positive rate 5.01% above 5%"},
		{"detection exactly 80%", Counts{Placebos: 200, Injected: 200, Detected: 160},
			true, ""},
		{"detection 79%", Counts{Placebos: 200, Injected: 200, Detected: 158}, false,
			"detection 79% below 80%"},
		{"nothing injected", Counts{Placebos: 200}, false, "detection 0% below 80%"},
		{"zero counts", Counts{}, false, "0 usable placebos below 200"},
	}
	for _, tc := range cases {
		ok, reason := Gate("v2", tc.c)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("%s: Gate = %v %q, want %v %q", tc.name, ok, reason, tc.ok, tc.reason)
		}
	}
}

func TestGate_RefusesAnotherVerifier(t *testing.T) {
	ok, reason := Gate("v1", passing())
	if ok || !strings.Contains(reason, `"v1"`) {
		t.Fatalf("Gate(v1) = %v %q, want refused naming v1", ok, reason)
	}
	if ok, _ := Gate("", passing()); ok {
		t.Fatal("Gate with no verifier passed")
	}
}

func TestPctAndWilson(t *testing.T) {
	for share, want := range map[float64]string{0: "0%", 0.05: "5%", 0.0501: "5.01%",
		1: "100%", 0.79: "79%", 1.0 / 3: "33.33%"} {
		if got := Pct(share); got != want {
			t.Errorf("Pct(%v) = %q, want %q", share, got, want)
		}
	}
	if got := WilsonUpper(0, 0); got != 1 {
		t.Errorf("WilsonUpper(0, 0) = %v, want 1 (no evidence)", got)
	}
	if got := WilsonUpper(0, 300); math.Abs(got-0.01264) > 0.0001 {
		t.Errorf("WilsonUpper(0, 300) = %v, want about 0.0126", got)
	}
	if got := WilsonUpper(15, 300); math.Abs(got-0.0811) > 0.001 {
		t.Errorf("WilsonUpper(15, 300) = %v, want about 0.081", got)
	}
	if got := WilsonUpper(300, 300); got != 1 {
		t.Errorf("WilsonUpper(300, 300) = %v, want 1", got)
	}
}

// noBlocks is a history where v2 can never measure: no blocks at all.
func noBlocks(t *testing.T) *Memory {
	t.Helper()
	m, err := FixtureAt(FixtureSeasonalWeek, FixtureStart, 30, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ivs := map[int64][]Interval{}
	for _, s := range m.Stmts {
		for _, iv := range m.Intervals(s.QueryID) {
			iv.Blocks = 0
			ivs[s.QueryID] = append(ivs[s.QueryID], iv)
		}
	}
	return NewMemory("no-blocks", FixtureStart, m.Stmts, nil, ivs, nil)
}

// A v2 that always says insufficient_evidence has rate 0 and detection 0:
// the gate refuses it on the detection floor, and the report says the
// inconclusive share is 100%.
func TestGate_VerifierThatNeverDecidesIsRefused(t *testing.T) {
	cfg := fixtureConfig("no-blocks", 1)
	cfg.Placebos = 220
	cfg.Now = func() time.Time { return FixtureStart.Add(30 * day) }
	r, err := Run(context.Background(), noBlocks(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := r.V2
	if c.FalsePositives != 0 || c.Detected != 0 || c.Inconclusive != c.Usable() ||
		c.Usable() != 220 {
		t.Fatalf("counts %+v, want 220 usable, all inconclusive, none detected", c)
	}
	ok, reason := r.Gate()
	if ok || reason != "detection 0% below 80%" {
		t.Fatalf("Gate = %v %q, want refused: detection 0%% below 80%%", ok, reason)
	}
	text := r.Text()
	for _, want := range []string{"inconclusive share 100%", "rate 0%",
		"gate: refused: detection 0% below 80%"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

// listReader is the latest of results, as the store's reader gives it.
type listReader struct{ results []Result }

func (l *listReader) LatestBacktest(context.Context, string) (verify.Backtest, bool,
	error) {
	if len(l.results) == 0 {
		return verify.Backtest{}, false, nil
	}
	return l.results[len(l.results)-1].Backtest(), true, nil
}

// A passing row flips auto to v2; a later failing row flips it back to
// shadow (once the resolver's cache expires).
func TestGate_AutoFollowsTheLatestRow(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	reader := &listReader{}
	res := verify.NewMethodResolver(reader, clock)
	resolve := func(want string) {
		t.Helper()
		got, err := res.Resolve(context.Background(), verify.MethodAuto, "db")
		if err != nil || got.Method != want {
			t.Fatalf("Resolve = %+v %v, want %s", got, err, want)
		}
	}
	resolve(verify.MethodShadow)
	reader.results = append(reader.results, Result{Verifier: "v2", V2: passing(),
		RanAt: now.Add(-time.Hour)})
	now = now.Add(verify.MethodCacheTTL + time.Second)
	resolve(verify.MethodV2)
	failing := passing()
	failing.Detected = 100
	reader.results = append(reader.results, Result{Verifier: "v2", V2: failing,
		RanAt: now})
	now = now.Add(verify.MethodCacheTTL + time.Second)
	resolve(verify.MethodShadow)
	reader.results = append(reader.results, Result{Verifier: "v2", V2: passing(),
		RanAt: now.Add(-36 * day)})
	now = now.Add(verify.MethodCacheTTL + time.Second)
	resolve(verify.MethodShadow)
}
