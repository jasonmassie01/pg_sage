package probes

import (
	"testing"
	"time"
)

// planhash v2 (collection-09) changes every fingerprint once, and a v2u
// (unresolved partition roots) fingerprint is not comparable with a v2
// one. plan_regressions v2 compares a sample only with the previous
// sample of the same fingerprint version.
func TestPlanRegressions_FingerprintVersionChangeIsNoFlip(t *testing.T) {
	pool, ctx := livePool(t)
	bootstrapQueryStore(t, ctx, pool)
	const upgradeQ, realQ, mixedQ, mixedSteadyQ = 881101, 881102, 881103, 881104
	seedSamples(t, ctx, pool, upgradeQ, []sample{
		{40 * time.Minute, 0, 0, "v1:a"}, {30 * time.Minute, 10, 10, "v1:a"},
		{20 * time.Minute, 20, 60, "v2:x"}, {10 * time.Minute, 30, 110, "v2:x"}})
	seedSamples(t, ctx, pool, realQ, []sample{
		{40 * time.Minute, 0, 0, "v2:x"}, {30 * time.Minute, 10, 10, "v2:x"},
		{20 * time.Minute, 20, 60, "v2:y"}, {10 * time.Minute, 30, 110, "v2:y"}})
	// An unresolved capture between two resolved ones neither hides the
	// real flip nor creates two false ones.
	seedSamples(t, ctx, pool, mixedQ, []sample{
		{40 * time.Minute, 0, 0, "v2:x"}, {30 * time.Minute, 10, 10, "v2:x"},
		{20 * time.Minute, 20, 20, "v2u:z"}, {10 * time.Minute, 30, 80, "v2:y"}})
	seedSamples(t, ctx, pool, mixedSteadyQ, []sample{
		{40 * time.Minute, 0, 0, "v2:x"}, {30 * time.Minute, 10, 10, "v2:x"},
		{20 * time.Minute, 20, 20, "v2u:z"}, {10 * time.Minute, 30, 30, "v2:x"}})

	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, PlanRegressions, Args{})
	if res.Version != "v2" {
		t.Fatalf("plan_regressions version = %q, want v2", res.Version)
	}
	shifts, err := PlanShifts(res)
	if err != nil {
		t.Fatalf("plan_regressions: %+v (%v)", res, err)
	}
	if up := shiftFor(t, shifts, upgradeQ); up.Flipped || up.PreviousHash != "" ||
		up.CurrentHash != "v2:x" {
		t.Errorf("v1 to v2 upgrade reported as a plan flip: %+v", up)
	}
	real := shiftFor(t, shifts, realQ)
	if !real.Flipped || real.PreviousHash != "v2:x" || real.CurrentHash != "v2:y" ||
		real.BeforeCalls != 10 || real.AfterCalls != 20 || real.AfterMeanMS != 5 {
		t.Errorf("v2 to v2 flip = %+v, want one flip x to y", real)
	}
	mixed := shiftFor(t, shifts, mixedQ)
	if !mixed.Flipped || mixed.PreviousHash != "v2:x" || mixed.CurrentHash != "v2:y" {
		t.Errorf("flip across an unresolved capture = %+v, want x to y", mixed)
	}
	if steady := shiftFor(t, shifts, mixedSteadyQ); steady.Flipped {
		t.Errorf("v2:x, v2u:z, v2:x reported as a flip: %+v", steady)
	}
}
