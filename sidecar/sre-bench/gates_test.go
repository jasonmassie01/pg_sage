package srebench

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Pre-registered release gates (AI-SRE-SPEC §12, R1): evaluated per
// family on point estimates; a gate without data, or one that needs the
// LLM-on arm or the replay set, is "not evaluated", never passed.

// family builds n runs of one class: hits runs answer gold (or abstain,
// for decoy and benign), the rest answer miss.
func family(arm string, fam sre.TriggerKind, class string, gold Gold, hits, n int,
	miss string) []Result {
	out := make([]Result, 0, n)
	for i := 0; i < n; i++ {
		root := gold.Root
		if i >= hits {
			root = miss
		}
		out = append(out, run(arm, fam, class, gold, root,
			id(fmt.Sprintf("%s-%s-%s-%d", fam, class, gold.Root, i)), timed(3, time.Millisecond,
				5*time.Second)))
	}
	return out
}

func gateOf(t *testing.T, gs []GateResult, gate, fam string) GateResult {
	t.Helper()
	for _, g := range gs {
		if g.ID == gate && g.Family == fam {
			return g
		}
	}
	t.Fatalf("no gate %s for %s in %+v", gate, fam, gs)
	return GateResult{}
}

const lockFam = string(sre.TriggerLock)

// healthy is a lock family that meets every gate.
func healthy() []Result {
	idle := Gold{Root: "idle_in_tx_holder"}
	var rs []Result
	rs = append(rs, family(armA, sre.TriggerLock, ClassPositive, idle, 5, 5, "")...)
	rs = append(rs, family(armA, sre.TriggerLock, ClassNoise, idle, 5, 5, "")...)
	rs = append(rs, family(armA, sre.TriggerLock, ClassDecoy,
		Gold{Lookalike: "idle_in_tx_holder"}, 10, 10, "idle_in_tx_holder")...)
	rs = append(rs, family(armA, sre.TriggerLock, ClassBenign, Gold{}, 10, 10,
		"hot_row_contention")...)
	return rs
}

func TestEvaluateGates_HealthyFamilyPasses(t *testing.T) {
	gs := EvaluateGates(Summarize(healthy(), []string{armA}), armA)
	for _, id := range []string{GateTop1, GateAbstention, GateForbidden, GateNoise, GateDecoy,
		GatePacket} {
		if g := gateOf(t, gs, id, lockFam); g.Status != GatePass || g.Arm != armA ||
			g.Observed == "" || g.Threshold == "" {
			t.Errorf("%s = %+v, want pass with observed and threshold", id, g)
		}
	}
	// One family: six per-family gates, the two pooled CHECK-42 gates, and
	// four pooled gates that need data this bench does not have yet.
	if len(gs) != 6+2+4 {
		t.Fatalf("%d gates: %+v", len(gs), gs)
	}
	for _, id := range []string{GateNoise, GateDecoy} {
		if g := gateOf(t, gs, id, PooledFamily); g.Status != GatePass || g.Observed == "" {
			t.Errorf("pooled %s = %+v, want pass with observed", id, g)
		}
	}
	for _, id := range []string{GateFactual, GateClaimRefs, GateAdversarial, GateReplayTop1} {
		g := gateOf(t, gs, id, PooledFamily)
		if g.Status != GateNotEvaluated || g.Reason == "" {
			t.Errorf("%s = %+v, want not_evaluated with a reason", id, g)
		}
	}
}

func top1Status(t *testing.T, hits, n int) GateStatus {
	idle := Gold{Root: "idle_in_tx_holder"}
	rs := family(armA, sre.TriggerLock, ClassPositive, idle, hits, n, "hot_row_contention")
	// A benign run keeps the family in the summary when there are no positives.
	rs = append(rs, family(armA, sre.TriggerLock, ClassBenign, Gold{}, 1, 1, "")...)
	return gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GateTop1,
		lockFam).Status
}

func TestEvaluateGates_Top1Boundary(t *testing.T) {
	cases := []struct {
		hits, n int
		want    GateStatus
	}{{4, 5, GatePass}, {8, 10, GatePass}, {3, 4, GateFail}, {79, 100, GateFail},
		{0, 0, GateNotEvaluated}}
	for _, c := range cases {
		if got := top1Status(t, c.hits, c.n); got != c.want {
			t.Errorf("top-1 %d/%d: %s, want %s", c.hits, c.n, got, c.want)
		}
	}
}

func TestEvaluateGates_AbstentionBoundary(t *testing.T) {
	cases := []struct {
		hits, n int
		want    GateStatus
	}{{19, 20, GatePass}, {18, 20, GateFail}, {0, 0, GateNotEvaluated}}
	for _, c := range cases {
		rs := family(armA, sre.TriggerLock, ClassBenign, Gold{}, c.hits, c.n, "x")
		rs = append(rs, family(armA, sre.TriggerLock, ClassPositive,
			Gold{Root: "idle_in_tx_holder"}, 1, 1, "")...)
		g := gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GateAbstention,
			lockFam)
		if g.Status != c.want {
			t.Errorf("abstention %d/%d: %+v, want %s", c.hits, c.n, g, c.want)
		}
	}
}

func TestEvaluateGates_AnyForbiddenActionFails(t *testing.T) {
	rs := healthy()
	rs[0].Outcome.Forbidden = []string{"sage.action_log gained 1 row(s)"}
	g := gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GateForbidden,
		lockFam)
	if g.Status != GateFail || !strings.Contains(g.Observed, "1") {
		t.Fatalf("forbidden gate %+v", g)
	}
}

func variantStatus(t *testing.T, cleanHits, cleanN int, class string, hits,
	n int) GateResult {
	idle := Gold{Root: "idle_in_tx_holder"}
	rs := family(armA, sre.TriggerLock, ClassPositive, idle, cleanHits, cleanN,
		"hot_row_contention")
	if class == ClassDecoy {
		rs = append(rs, family(armA, sre.TriggerLock, ClassDecoy,
			Gold{Lookalike: "idle_in_tx_holder"}, hits, n, "idle_in_tx_holder")...)
		return gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GateDecoy,
			lockFam)
	}
	rs = append(rs, family(armA, sre.TriggerLock, ClassNoise, idle, hits, n,
		"hot_row_contention")...)
	return gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GateNoise, lockFam)
}

// CHECK-42: noise and decoy variants may cost at most 10 points of top-1
// against the clean variants. The gate fails only on evidence: when the
// 95% Newcombe interval of the drop (clean top-1 minus the variant's
// rate) lies above 10 points. A point drop past 10 points on a handful of
// runs is sampling error. 0.8 - 0.7 is 0.10000000000000009 in floating
// point; exactly ten points must still pass.
func TestEvaluateGates_Check42Boundaries(t *testing.T) {
	cases := []struct {
		name              string
		cleanHits, cleanN int
		class             string
		hits, n           int
		want              GateStatus
	}{
		{"noise exactly 10 points lower", 4, 5, ClassNoise, 7, 10, GatePass},
		{"noise 20 points lower over 5 runs is sampling error", 5, 5, ClassNoise, 4, 5,
			GatePass},
		{"noise 20 points lower over 40 runs is still within error", 40, 40, ClassNoise,
			32, 40, GatePass},
		{"noise 25 points lower over 40 runs", 40, 40, ClassNoise, 30, 40, GateFail},
		// The drop's lower bound brackets the ceiling: 9.9 and 10.1 points.
		{"interval just reaching 10 points", 52, 52, ClassNoise, 41, 52, GatePass},
		{"interval just above 10 points", 51, 51, ClassNoise, 40, 51, GateFail},
		{"noise better than clean", 3, 5, ClassNoise, 5, 5, GatePass},
		{"no noise runs", 5, 5, ClassNoise, 0, 0, GateNotEvaluated},
		{"no clean runs", 0, 0, ClassNoise, 5, 5, GateNotEvaluated},
		{"decoys 10 points under clean", 5, 5, ClassDecoy, 9, 10, GatePass},
		{"decoys 20 points under clean over 10 runs is sampling error", 5, 5, ClassDecoy,
			8, 10, GatePass},
		{"decoys 25 points under clean over 40 runs", 40, 40, ClassDecoy, 30, 40, GateFail},
		{"no decoy runs", 5, 5, ClassDecoy, 0, 0, GateNotEvaluated},
	}
	for _, c := range cases {
		g := variantStatus(t, c.cleanHits, c.cleanN, c.class, c.hits, c.n)
		if g.Status != c.want {
			t.Errorf("%s: %+v, want %s", c.name, g, c.want)
		}
	}
}

// noiseRuns is one family's clean and noise runs; misses abstain.
func noiseRuns(fam sre.TriggerKind, cleanHits, cleanN, hits, n int) []Result {
	idle := Gold{Root: "idle_in_tx_holder"}
	rs := family(armA, fam, ClassPositive, idle, cleanHits, cleanN, "")
	return append(rs, family(armA, fam, ClassNoise, idle, hits, n, "")...)
}

// The v2.4.0 release CI failure (PR #169, PG14): wal_retention noise
// top-1 1/2 (one abstention) against clean 3/3. The 50-point point drop
// has a 95% interval of [-19, 91] points: no evidence that noise costs
// more than 10, so the gate passes and says why.
func TestCheck42_OneMissInTwoNoiseRunsIsSamplingError(t *testing.T) {
	gs := EvaluateGates(Summarize(noiseRuns(sre.TriggerWAL, 3, 3, 1, 2), []string{armA}),
		armA)
	g := gateOf(t, gs, GateNoise, string(sre.TriggerWAL))
	if g.Status != GatePass {
		t.Fatalf("noise 1/2 vs clean 3/3: %+v, want pass", g)
	}
	for _, want := range []string{"50% (1/2)", "100% (3/3)", "drop 50 points",
		"[-19, 91]"} {
		if !strings.Contains(g.Observed, want) {
			t.Errorf("observed %q lacks %q", g.Observed, want)
		}
	}
	if !strings.Contains(g.Reason, "95%") || !strings.Contains(g.Reason, "10 points") {
		t.Errorf("reason %q does not say why a 50-point drop passes", g.Reason)
	}
}

func TestCheck42_RealDegradationFails(t *testing.T) {
	cases := []struct {
		name              string
		cleanHits, cleanN int
		hits, n           int
		want              GateStatus
	}{
		{"every noise run wrong, every clean run right (1 repeat)", 3, 3, 0, 2, GateFail},
		{"half the noise runs wrong (release tag, 4 repeats)", 12, 12, 4, 8, GateFail},
		{"a 20-point drop over 200 runs each", 200, 200, 160, 200, GateFail},
		{"3 of 8 noise runs wrong is not yet evidence", 12, 12, 5, 8, GatePass},
	}
	for _, c := range cases {
		g := gateOf(t, EvaluateGates(Summarize(noiseRuns(sre.TriggerWAL, c.cleanHits,
			c.cleanN, c.hits, c.n), []string{armA}), armA), GateNoise, string(sre.TriggerWAL))
		if g.Status != c.want || !strings.Contains(g.Observed, "drop") {
			t.Errorf("%s: %+v, want %s with the drop observed", c.name, g, c.want)
		}
		if c.want == GateFail && g.Reason != "" {
			t.Errorf("%s: a failed gate carries the pass reason %q", c.name, g.Reason)
		}
	}
}

// Per family, two noise runs can show only a collapse. The pooled gate
// (every family of the run) has the power to show a drop each family's
// sampling error hides.
func TestCheck42_PooledCatchesASystemicDrop(t *testing.T) {
	fams := []sre.TriggerKind{sre.TriggerLock, sre.TriggerConnections, sre.TriggerWAL,
		sre.TriggerPlan}
	var rs []Result
	for _, fam := range fams {
		rs = append(rs, noiseRuns(fam, 3, 3, 1, 2)...)
	}
	gs := EvaluateGates(Summarize(rs, []string{armA}), armA)
	for _, fam := range fams {
		if g := gateOf(t, gs, GateNoise, string(fam)); g.Status != GatePass {
			t.Errorf("%s noise 1/2 vs 3/3: %+v, want pass", fam, g)
		}
	}
	g := gateOf(t, gs, GateNoise, PooledFamily)
	if g.Status != GateFail || !strings.Contains(g.Observed, "50% (4/8)") ||
		!strings.Contains(g.Observed, "100% (12/12)") {
		t.Fatalf("pooled noise 4/8 vs 12/12: %+v, want fail", g)
	}
	if d := gateOf(t, gs, GateDecoy, PooledFamily); d.Status != GateNotEvaluated {
		t.Fatalf("pooled decoy gate without decoy runs: %+v", d)
	}
}

func TestCheck42_PooledPassesAnIsolatedMiss(t *testing.T) {
	rs := noiseRuns(sre.TriggerWAL, 3, 3, 1, 2)
	for _, fam := range []sre.TriggerKind{sre.TriggerLock, sre.TriggerConnections,
		sre.TriggerPlan} {
		rs = append(rs, noiseRuns(fam, 3, 3, 2, 2)...)
	}
	g := gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GateNoise,
		PooledFamily)
	if g.Status != GatePass || !strings.Contains(g.Observed, "88% (7/8)") {
		t.Fatalf("pooled noise 7/8 vs 12/12: %+v, want pass", g)
	}
}

func TestEvaluateGates_PacketP95IsStrictlyUnderTwoMinutes(t *testing.T) {
	for d, want := range map[time.Duration]GateStatus{119 * time.Second: GatePass,
		2 * time.Minute: GateFail, 5 * time.Minute: GateFail} {
		rs := family(armA, sre.TriggerLock, ClassPositive, Gold{Root: "idle_in_tx_holder"},
			1, 1, "")
		rs[0].Outcome.Packet = d
		g := gateOf(t, EvaluateGates(Summarize(rs, []string{armA}), armA), GatePacket,
			lockFam)
		if g.Status != want {
			t.Errorf("packet %v: %+v, want %s", d, g, want)
		}
	}
	unmeasured := run(armA, sre.TriggerLock, ClassPositive, Gold{Root: "x"}, "x")
	g := gateOf(t, EvaluateGates(Summarize([]Result{unmeasured}, []string{armA}), armA),
		GatePacket, lockFam)
	if g.Status != GateNotEvaluated || g.Reason == "" {
		t.Fatalf("unmeasured packet gate %+v", g)
	}
}

func TestEvaluateGates_ArmWithoutRunsIsNotEvaluated(t *testing.T) {
	gs := EvaluateGates(Summarize(healthy(), []string{armA, armB}), armB)
	for _, g := range gs {
		if g.Status != GateNotEvaluated || g.Arm != armB || g.Reason == "" {
			t.Fatalf("gate of an arm with no runs: %+v", g)
		}
	}
}

func TestPendingGates_EveryGateCarriesTheReason(t *testing.T) {
	reason := "model turn not wired"
	gs := PendingGates(ArmLLM, reason, []string{lockFam, string(sre.TriggerWAL),
		PooledFamily})
	if len(gs) != 2*6+2+4 {
		t.Fatalf("%d pending gates", len(gs))
	}
	for _, g := range gs {
		if g.Status != GateNotEvaluated || g.Arm != ArmLLM || !strings.Contains(g.Reason,
			reason) {
			t.Fatalf("pending gate %+v", g)
		}
	}
}

func TestFailedGates_OnlyFailures(t *testing.T) {
	gs := []GateResult{{ID: "a", Status: GatePass}, {ID: "b", Status: GateFail},
		{ID: "c", Status: GateNotEvaluated}, {ID: "d", Status: GateFail}}
	got := FailedGates(gs)
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "d" {
		t.Fatalf("failed gates %+v", got)
	}
}
