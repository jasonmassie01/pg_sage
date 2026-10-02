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
	// One family: six per-family gates plus four pooled gates that need data
	// this bench does not have yet.
	if len(gs) != 6+4 {
		t.Fatalf("%d gates: %+v", len(gs), gs)
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
// against the clean variants. 0.8 - 0.7 is 0.10000000000000009 in
// floating point; exactly ten points must still pass.
func TestEvaluateGates_Check42Boundaries(t *testing.T) {
	cases := []struct {
		name              string
		cleanHits, cleanN int
		class             string
		hits, n           int
		want              GateStatus
	}{
		{"noise exactly 10 points lower", 4, 5, ClassNoise, 7, 10, GatePass},
		{"noise 20 points lower", 5, 5, ClassNoise, 4, 5, GateFail},
		{"noise better than clean", 3, 5, ClassNoise, 5, 5, GatePass},
		{"no noise runs", 5, 5, ClassNoise, 0, 0, GateNotEvaluated},
		{"no clean runs", 0, 0, ClassNoise, 5, 5, GateNotEvaluated},
		{"decoys 10 points under clean", 5, 5, ClassDecoy, 9, 10, GatePass},
		{"decoys 20 points under clean", 5, 5, ClassDecoy, 8, 10, GateFail},
		{"no decoy runs", 5, 5, ClassDecoy, 0, 0, GateNotEvaluated},
	}
	for _, c := range cases {
		g := variantStatus(t, c.cleanHits, c.cleanN, c.class, c.hits, c.n)
		if g.Status != c.want {
			t.Errorf("%s: %+v, want %s", c.name, g, c.want)
		}
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
	if len(gs) != 2*6+4 {
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
