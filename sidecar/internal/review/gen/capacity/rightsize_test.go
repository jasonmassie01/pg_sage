package capacity

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cost"
)

// downsizeVerdict is a 4-to-2 vCPU downsize; delta is its monthly compute
// change (nil: no rates).
func downsizeVerdict(delta *float64) cost.Verdict {
	v := cost.Verdict{Decision: cost.DecisionDownsize, Reason: "CPU p95 20% projects to 40%",
		CurrentClass: "db.r6g.xlarge", TargetClass: "db.r6g.large", CurrentVCPUs: 4,
		TargetVCPUs: 2, CPUP95: 20, CPUMax: 30, ProjectedCPUP95: 40, ProjectedCPUMax: 60,
		Days: 30, Coverage: 1}
	if delta != nil {
		v.MonthlyDelta, v.Currency, v.RateSource = delta, "USD", cost.SourceConfig
		v.AsOf = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	}
	return v
}

func TestBuild_RightSize_NoRates_UnknownCostAndNoDollars(t *testing.T) {
	rs := &Rightsizing{Resource: "db-1", Verdict: downsizeVerdict(nil)}
	cands, cov, err := Build(steadyPack(t), Inputs{Rightsizing: rs}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	got := byKind(cands, KindRightSize)
	if len(got) != 1 {
		t.Fatalf("%d right-size candidates, want 1: %+v", len(got), cands)
	}
	c := got[0]
	if c.ActionType != "advice" || c.Approval != "approval_required" ||
		c.Target != "instance:db-1" || c.ForwardSQL != "" {
		t.Fatalf("right-size candidate %+v: never a step pg_sage runs", c)
	}
	for _, want := range []string{"db.r6g.xlarge", "db.r6g.large", NoRates} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("right-size reason lacks %q: %s", want, c.Reason)
		}
	}
	if c.Prediction.Quantities[QuantityTargetVCPUs] != 2 {
		t.Fatalf("quantities %v", c.Prediction.Quantities)
	}
	requireNoDollars(t, cands, cov)
}

func TestBuild_RightSize_WithRates_StatesTheMonthlyDelta(t *testing.T) {
	delta := -1460.0
	rs := &Rightsizing{Resource: "db-1", Verdict: downsizeVerdict(&delta)}
	cands, _, err := Build(steadyPack(t), Inputs{Rightsizing: rs}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	got := byKind(cands, KindRightSize)
	if len(got) != 1 {
		t.Fatalf("%d right-size candidates", len(got))
	}
	if q := got[0].Prediction.Quantities[QuantityMonthlyDelta]; q != -1460 {
		t.Fatalf("monthly delta %v, want -1460", q)
	}
	for _, want := range []string{"-1460.00 USD", "2026-10-01", "config"} {
		if !strings.Contains(got[0].Reason, want) {
			t.Errorf("right-size reason lacks %q: %s", want, got[0].Reason)
		}
	}
}

// zeroVCPUHours is 20 days of hourly usage on a size with no vCPU count.
func zeroVCPUHours(now time.Time) []cost.HourRow {
	rows := make([]cost.HourRow, 0, 480)
	p95 := 10.0
	for h := 480; h > 0; h-- {
		rows = append(rows, cost.HourRow{Hour: now.Add(-time.Duration(h) * time.Hour),
			InstanceClass: "custom-x", VCPUs: 0, CPUP95: &p95, CPUMax: &p95, Samples: 60})
	}
	return rows
}

func TestBuild_ZeroVCPURatesAreRefusedByCostAndTheReasonCarried(t *testing.T) {
	now := window.To
	rates := cost.Rates{Source: cost.SourceConfig, Currency: "USD", ComputePerHour: 2,
		VCPUs: 0}
	v := cost.Rightsize(cost.RightsizeInput{Resource: "db-1", Rows: zeroVCPUHours(now),
		Now: now, Rates: &rates})
	if v.Decision != cost.DecisionUnknown {
		t.Fatalf("cost judged a size with no vCPU count: %+v", v)
	}
	rs := &Rightsizing{Resource: "db-1", Verdict: v}
	cands, cov, err := Build(steadyPack(t), Inputs{Rightsizing: rs, Rates: &rates},
		testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind(cands, KindRightSize)) != 0 {
		t.Fatalf("an unknown verdict became a candidate: %+v", cands)
	}
	if !hasNote(cov.Notes, "db-1", v.Reason) || !contains(cov.Missing, MissingRightsize) {
		t.Fatalf("coverage %+v does not carry %q", cov, v.Reason)
	}
}

func TestBuild_RightsizeErrorIsCarriedAsUnknownWithItsText(t *testing.T) {
	err := errors.New("read shared_buffers for right-sizing db-1: permission denied")
	cands, cov, berr := Build(steadyPack(t), Inputs{RightsizeErr: err}, testEnv())
	if berr != nil {
		t.Fatalf("a right-size error failed the review: %v", berr)
	}
	if len(byKind(cands, KindRightSize)) != 0 {
		t.Fatal("a right-size candidate from an error")
	}
	if !hasNote(cov.Notes, "unknown: "+err.Error()) || !contains(cov.Missing,
		MissingRightsize) {
		t.Fatalf("coverage %+v does not carry the error as unknown", cov)
	}
}

func TestBuild_KeepIsANoteNotACandidate(t *testing.T) {
	v := cost.Verdict{Decision: cost.DecisionKeep, Reason: "no smaller size in the family",
		CurrentClass: "db.t4g.micro", CurrentVCPUs: 2}
	cands, cov, err := Build(steadyPack(t), Inputs{Rightsizing: &Rightsizing{
		Resource: "db-1", Verdict: v}}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind(cands, KindRightSize)) != 0 {
		t.Fatal("keep became a candidate")
	}
	if !hasNote(cov.Notes, "keep db.t4g.micro", v.Reason) || contains(cov.Missing,
		MissingRightsize) {
		t.Fatalf("coverage %+v", cov)
	}
}

func TestBuild_StorageHeadroom_OnlyWhenTheNinetyDayProjectionNearsTheAllocation(t *testing.T) {
	keep := cost.Verdict{Decision: cost.DecisionKeep, Reason: "fits"}
	tight := &Rightsizing{Resource: "db-1", Verdict: keep, AllocatedStorageBytes: 220 * gib}
	cands, _, err := Build(steadyPack(t), Inputs{Rightsizing: tight}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	got := byKind(cands, KindStorageHeadroom)
	if len(got) != 1 || got[0].ActionType != "advice" || got[0].Target != "instance:db-1" {
		t.Fatalf("storage headroom with 217 GiB projected of 220: %+v", got)
	}
	q := got[0].Prediction.Quantities
	if q[QuantityAllocatedBytes] != 220*gib || math.Abs(q[QuantityProjected90]-217*gib) >
		0.01*217*gib {
		t.Fatalf("storage quantities %v", q)
	}
	roomy := &Rightsizing{Resource: "db-1", Verdict: keep, AllocatedStorageBytes: 1024 * gib}
	cands, _, err = Build(steadyPack(t), Inputs{Rightsizing: roomy}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(byKind(cands, KindStorageHeadroom)) != 0 {
		t.Fatal("storage headroom advice with 1 TiB allocated")
	}
}

// fakeRightsizer returns its result.
type fakeRightsizer struct {
	rs  Rightsizing
	err error
}

func (f fakeRightsizer) Rightsize(context.Context) (Rightsizing, error) { return f.rs, f.err }

func TestGenerate_ARightsizerErrorIsCarriedNotFatal(t *testing.T) {
	g := New(Config{Rightsizer: fakeRightsizer{err: errors.New("cloud API timed out")}})
	cands, cov, err := g.Generate(context.Background(), steadyPack(t), testEnv())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(byKind(cands, KindGrowthPlan)) != 1 {
		t.Fatalf("the growth plan was lost with the right-size error: %+v", cands)
	}
	if !hasNote(cov.Notes, "unknown: cloud API timed out") {
		t.Fatalf("notes %v", cov.Notes)
	}
}

func TestCostRightsizer_EvaluateRightsizeErrorIsReturned(t *testing.T) {
	// No pool: cost.EvaluateRightsize refuses before any read.
	r := CostRightsizer{Resource: "db-1", Database: "app"}
	_, err := r.Rightsize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "right-sizing needs a database") {
		t.Fatalf("err %v, want cost's refusal", err)
	}
	g := New(Config{Rightsizer: r})
	_, cov, gerr := g.Generate(context.Background(), steadyPack(t), testEnv())
	if gerr != nil || !hasNote(cov.Notes, "unknown: ", "right-sizing needs a database") {
		t.Fatalf("generate %v, notes %v", gerr, cov.Notes)
	}
}

func TestGenerate_NoCloudAdapterSaysSo(t *testing.T) {
	_, cov, err := New(Config{}).Generate(context.Background(), steadyPack(t), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if !hasNote(cov.Notes, "right-size", "no cloud provider resource") {
		t.Fatalf("notes %v", cov.Notes)
	}
}
