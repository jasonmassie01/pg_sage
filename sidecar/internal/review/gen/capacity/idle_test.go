package capacity

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cost"
)

func TestIdle_ThirteenQuietDaysIsNotIdleFourteenIs(t *testing.T) {
	thirteen := quiet(30, 17)
	if got := QuietDays(thirteen); got != 13 || Idle(thirteen) {
		t.Fatalf("13 quiet days: QuietDays %d Idle %v, want 13 and not idle", got,
			Idle(thirteen))
	}
	fourteen := quiet(30, 16)
	if got := QuietDays(fourteen); got != 14 || !Idle(fourteen) {
		t.Fatalf("14 quiet days: QuietDays %d Idle %v, want 14 and idle", got,
			Idle(fourteen))
	}
}

func TestIdle_AnUnobservedDayEndsTheQuietRun(t *testing.T) {
	days := quiet(30, 5)
	days[20].Observed = false // pg_sage saw nothing that day: no proof of quiet
	if got := QuietDays(days); got != 9 || Idle(days) {
		t.Fatalf("QuietDays %d Idle %v, want 9 (the run stops at the gap)", got, Idle(days))
	}
	days = quiet(20, 0)
	days[19].Observed = false
	if got := QuietDays(days); got != 0 {
		t.Fatalf("an unobserved last day gave %d quiet days, want 0", got)
	}
}

func TestIdle_UnorderedDaysAreSortedFirst(t *testing.T) {
	days := quiet(20, 3)
	days[0], days[19] = days[19], days[0]
	if got := QuietDays(days); got != 17 {
		t.Fatalf("QuietDays of unordered days %d, want 17", got)
	}
}

func TestIdle_NilEmptyAndNeverCalled(t *testing.T) {
	if QuietDays(nil) != 0 || Idle(nil) || Idle([]DayCalls{}) {
		t.Fatal("no days were judged idle")
	}
	never := quiet(20, 0)
	if QuietDays(never) != 20 || !Idle(never) {
		t.Fatalf("20 observed days without a call: QuietDays %d", QuietDays(never))
	}
}

func TestBuild_IdleDatabase_NoRates_NoDollarFigure(t *testing.T) {
	pack := steadyPack(t)
	cands, cov, err := Build(pack, Inputs{Days: quiet(20, 2)}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	idle := byKind(cands, KindIdleDatabase)
	if len(idle) != 1 {
		t.Fatalf("%d idle candidates, want 1: %+v", len(idle), cands)
	}
	c := idle[0]
	if c.Target != "database:app" || c.ActionType != "advice" ||
		c.Prediction.Quantities[QuantityQuietDays] != 18 ||
		c.Prediction.Quantities[QuantitySizeBytes] <= 0 {
		t.Fatalf("idle candidate %+v", c)
	}
	for _, want := range []string{"18 days", NoRates, "GiB"} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("idle reason lacks %q: %s", want, c.Reason)
		}
	}
	requireNoDollars(t, cands, cov)
}

func TestBuild_IdleDatabase_WithRates_StatesItsMonthlyStorageCost(t *testing.T) {
	db := series(dbID, "", linear(28, 50*gib, 0, 0))
	pack := growthPack(t, db)
	rates := &cost.Rates{Source: cost.SourceConfig, Currency: "USD", StorageGBMonth: 0.10,
		AsOf: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ComputePerHour: 1}
	cands, _, err := Build(pack, Inputs{Days: quiet(20, 0), Rates: rates}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	idle := byKind(cands, KindIdleDatabase)
	if len(idle) != 1 {
		t.Fatalf("%d idle candidates, want 1", len(idle))
	}
	got := idle[0].Prediction.Quantities[QuantityMonthlyCost]
	if math.Abs(got-5.0) > 1e-9 {
		t.Fatalf("monthly cost %v, want 5.00 (50 GiB at 0.10 a GiB-month)", got)
	}
	for _, want := range []string{"5.00 USD", "2026-10-01", "config"} {
		if !strings.Contains(idle[0].Reason, want) {
			t.Errorf("idle reason lacks %q: %s", want, idle[0].Reason)
		}
	}
}

func TestBuild_IdleDatabase_RatesWithoutAStorageRate(t *testing.T) {
	rates := &cost.Rates{Source: cost.SourceConfig, Currency: "USD", ComputePerHour: 1}
	cands, _, err := Build(steadyPack(t), Inputs{Days: quiet(20, 0), Rates: rates},
		testEnv())
	if err != nil {
		t.Fatal(err)
	}
	idle := byKind(cands, KindIdleDatabase)
	if len(idle) != 1 || !strings.Contains(idle[0].Reason, "no storage rate") {
		t.Fatalf("idle without a storage rate %+v", idle)
	}
	if _, ok := idle[0].Prediction.Quantities[QuantityMonthlyCost]; ok {
		t.Fatal("a monthly cost was stated without a storage rate")
	}
}

func TestBuild_ActiveDatabaseIsNotIdle(t *testing.T) {
	cands, cov, err := Build(steadyPack(t), Inputs{Days: quiet(20, 7)}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind(cands, KindIdleDatabase)) != 0 {
		t.Fatalf("13 quiet days gave an idle candidate: %+v", cands)
	}
	if !hasNote(cov.Notes, "13 days") {
		t.Fatalf("coverage notes %v do not state the quiet days", cov.Notes)
	}
}

func TestBuild_NoActivityReadIsMissingNotIdle(t *testing.T) {
	cands, cov, err := Build(steadyPack(t), Inputs{}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind(cands, KindIdleDatabase)) != 0 {
		t.Fatal("an idle candidate without any activity read")
	}
	if !contains(cov.Missing, MissingActivity) || cov.Status != "partial" {
		t.Fatalf("coverage %+v, want %s missing", cov, MissingActivity)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
