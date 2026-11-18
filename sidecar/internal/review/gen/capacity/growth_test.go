package capacity

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestProject_LinearGiBPerDay_NinetyDaysWithinOnePercentWithBand(t *testing.T) {
	s := series(dbID, "", linear(28, 100*gib, gib, mib))
	p, err := Project(s, window.From)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if p.Points != 28 || !p.Origin.Equal(day(27)) {
		t.Fatalf("points %d origin %s, want 28 and %s", p.Points, p.Origin, day(27))
	}
	if math.Abs(p.SlopePerDay-gib)/gib > 0.01 {
		t.Fatalf("slope %.0f bytes/day, want about 1 GiB", p.SlopePerDay)
	}
	want := map[int]float64{30: 157 * gib, 90: 217 * gib, 365: 492 * gib}
	if len(p.Ahead) != 3 {
		t.Fatalf("%d horizons, want 30, 90 and 365", len(p.Ahead))
	}
	for _, a := range p.Ahead {
		w, ok := want[a.Days]
		if !ok {
			t.Fatalf("unexpected horizon %d", a.Days)
		}
		if math.Abs(a.Bytes-w)/w > 0.01 {
			t.Fatalf("%d days: %.2f GiB, want %.2f GiB within 1%%", a.Days, a.Bytes/gib,
				w/gib)
		}
		if a.Band <= 0 || a.Band > 0.01*w {
			t.Fatalf("%d days: band %.0f bytes, want a positive band under 1%%", a.Days,
				a.Band)
		}
	}
	a30, _ := p.At(30)
	a365, _ := p.At(365)
	if a365.Band <= a30.Band {
		t.Fatalf("the band does not widen with the horizon: %v then %v", a30.Band, a365.Band)
	}
	if p.RMSE <= 0 || p.RMSE > 2*mib {
		t.Fatalf("fit error %.0f bytes, want about the 1 MiB noise", p.RMSE)
	}
	if _, ok := p.At(60); ok {
		t.Fatal("At(60) answered a horizon the projection does not state")
	}
}

// The band is pinned to values worked out by hand (outside the code under
// test) for y = 10, 13, 11, 14, 12, 16, 15 on days 0..6: slope 11/14,
// residual standard error 1.46385, and twice the standard error of a new
// observation at each horizon.
func TestProject_BandIsTwiceThePredictionStandardError(t *testing.T) {
	ys := []float64{10, 13, 11, 14, 12, 16, 15}
	vals := make([]*float64, len(ys))
	for i := range ys {
		vals[i] = &ys[i]
	}
	p, err := Project(series(dbID, "", vals), window.From)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(p.SlopePerDay-11.0/14) > 1e-9 || math.Abs(p.RMSE-1.4638501094) > 1e-9 {
		t.Fatalf("slope %v rmse %v, want 11/14 and 1.4638501094", p.SlopePerDay, p.RMSE)
	}
	want := map[int][2]float64{30: {38.9285714286, 18.5246664020},
		90: {86.0714285714, 51.5504508185}, 365: {302.1428571429, 203.6323217198}}
	for h, w := range want {
		a, ok := p.At(h)
		if !ok || math.Abs(a.Bytes-w[0]) > 1e-6 || math.Abs(a.Band-w[1]) > 1e-6 {
			t.Fatalf("%d days: %+v (ok %v), want %v bytes +/- %v", h, a, ok, w[0], w[1])
		}
	}
}

func TestProject_ExactLineHasAZeroBand(t *testing.T) {
	p, err := Project(series(dbID, "", linear(10, 10*gib, gib, 0)), window.From)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := p.At(90)
	if !ok || math.Abs(a.Bytes-109*gib) > 1 || a.Band != 0 {
		t.Fatalf("exact line at 90 days = %+v (ok %v), want 109 GiB and no band", a, ok)
	}
}

func TestProject_Shrinking_NeverANegativeSize(t *testing.T) {
	s := series(dbID, "", linear(10, 10*gib, -gib, 0))
	p, err := Project(s, window.From)
	if err != nil {
		t.Fatal(err)
	}
	if p.SlopePerDay >= 0 || !p.Shrinking() {
		t.Fatalf("slope %v shrinking %v, want a negative fit", p.SlopePerDay, p.Shrinking())
	}
	for _, a := range p.Ahead {
		if a.Bytes != 0 || a.Band < 0 {
			t.Fatalf("%d days: %v bytes band %v, want 0 bytes (never negative)", a.Days,
				a.Bytes, a.Band)
		}
	}
	pack := growthPack(t, s)
	cands, _, err := Build(pack, Inputs{}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	plan := byKind(cands, KindGrowthPlan)
	if len(plan) != 1 || plan[0].Prediction.Direction != "decrease" ||
		!strings.Contains(plan[0].Reason, "shrinking") {
		t.Fatalf("a shrinking database's plan %+v", plan)
	}
}

func TestProject_SevenPointsIsTheFewest(t *testing.T) {
	all := linear(28, gib, gib, 0)
	_, err := Project(series(dbID, "", sparse(all, 0, 3, 7, 10, 20, 27)), window.From)
	if !errors.Is(err, ErrTooFewPoints) {
		t.Fatalf("6 points: err %v, want ErrTooFewPoints", err)
	}
	if !strings.Contains(err.Error(), "6 daily points") || !strings.Contains(err.Error(), "7") {
		t.Fatalf("the refusal %q does not say 6 points of the 7 needed", err)
	}
	p, err := Project(series(dbID, "", sparse(all, 0, 3, 7, 10, 20, 25, 27)), window.From)
	if err != nil || p.Points != 7 {
		t.Fatalf("7 points: %+v, %v; want a projection", p, err)
	}
	// A missing day keeps its x: the gaps do not bend the line.
	if a, _ := p.At(30); math.Abs(a.Bytes-58*gib) > 1 {
		t.Fatalf("30 days from day 27 = %.3f GiB, want 58", a.Bytes/gib)
	}
}

func TestProject_EmptyAndNil(t *testing.T) {
	for name, vals := range map[string][]*float64{"nil": nil, "empty": {},
		"all missing": make([]*float64, 28)} {
		if _, err := Project(series(dbID, "", vals), window.From); !errors.Is(err,
			ErrTooFewPoints) {
			t.Errorf("%s: err %v, want ErrTooFewPoints", name, err)
		}
	}
}

func TestProject_InvalidSizeIsRefused(t *testing.T) {
	vals := linear(10, gib, gib, 0)
	bad := -5.0
	vals[4] = &bad
	if _, err := Project(series(dbID, "", vals), window.From); err == nil ||
		!strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative size was projected: %v", err)
	}
	nan := math.NaN()
	vals[4] = &nan
	if _, err := Project(series(dbID, "", vals), window.From); err == nil {
		t.Fatal("a NaN size was projected")
	}
}

func TestBuild_GrowthPlanForTheDatabaseAndEachTable(t *testing.T) {
	pack := growthPack(t, series(dbID, "", linear(28, 100*gib, gib, mib)),
		series(tableID, "public.orders", linear(28, 10*gib, 0.1*gib, 0)))
	cands, cov, err := Build(pack, Inputs{Days: quiet(20, 20)}, testEnv())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	db := byKind(cands, KindGrowthPlan)
	tb := byKind(cands, KindTableGrowth)
	if len(db) != 1 || len(tb) != 1 {
		t.Fatalf("%d growth plans and %d table growths, want 1 and 1: %+v", len(db),
			len(tb), cands)
	}
	c := db[0]
	if c.Target != "database:app" || c.ActionType != "advice" || c.Approval !=
		"approval_required" || c.EvidenceIDs[0] != dbID || c.Dimension != Dimension {
		t.Fatalf("growth plan %+v", c)
	}
	q := c.Prediction.Quantities
	if math.Abs(q[QuantityProjected90]-217*gib)/(217*gib) > 0.01 || q[QuantityBand90] <= 0 ||
		q[QuantityPoints] != 28 || q[QuantityFitRMSE] <= 0 {
		t.Fatalf("growth plan quantities %v", q)
	}
	for _, want := range []string{"30 days", "90 days", "365 days", "GiB", "fit error"} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("growth plan reason lacks %q: %s", want, c.Reason)
		}
	}
	if tb[0].Target != "table:public.orders" || tb[0].EvidenceIDs[0] != tableID {
		t.Fatalf("table growth %+v", tb[0])
	}
	if len(cov.Missing) != 0 {
		t.Fatalf("coverage missing %v with every section present", cov.Missing)
	}
}

func TestBuild_TooFewPointsIsANoteNotACandidate(t *testing.T) {
	all := linear(28, gib, gib, 0)
	pack := growthPack(t, series(dbID, "", sparse(all, 1, 5, 9, 12, 27)))
	cands, cov, err := Build(pack, Inputs{}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind(cands, KindGrowthPlan)) != 0 {
		t.Fatalf("5 points gave a growth plan: %+v", cands)
	}
	if !hasNote(cov.Notes, dbID, "5 daily points") {
		t.Fatalf("coverage notes %v do not say why there is no projection", cov.Notes)
	}
}
