package sre

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE M6 reactive families: checkpoint storms, temp-file
// explosions, replication lag and LWLock contention. Each has a fixed
// probe plan under the probe ceiling (leaving room for the model turn's
// one probe), reads pg_sage's own actions (CHECK-38), and is diagnosed by
// its own causal-graph matcher.

var m6Kinds = []TriggerKind{TriggerCheckpoint, TriggerTempFiles, TriggerReplicationLag,
	TriggerLWLock}

func planProbes(plan []planStep) (n int, ids map[probes.ID]int) {
	ids = map[probes.ID]int{}
	for _, st := range plan {
		for _, c := range st.calls {
			n++
			ids[c.id]++
		}
	}
	return n, ids
}

func TestPlanFor_M6FamiliesFitTheProbeCeiling(t *testing.T) {
	ceiling := DefaultLimits().MaxProbes
	for _, kind := range m6Kinds {
		plan, ok := planFor(kind, time.Hour)
		if !ok || len(plan) < 2 {
			t.Fatalf("%s: plan = %+v (ok %v), want a sampled plan", kind, plan, ok)
		}
		n, ids := planProbes(plan)
		if n+1 > ceiling {
			t.Errorf("%s runs %d probes: no room for the model's probe under %d", kind,
				n, ceiling)
		}
		if ids[probes.SageActions] != 1 {
			t.Errorf("%s does not read pg_sage's own actions once (CHECK-38)", kind)
		}
		if plan[0].sample {
			t.Errorf("%s: the first step must not wait", kind)
		}
		for _, c := range plan[0].calls {
			if c.id == probes.SageActions && c.args.Window != time.Hour {
				t.Errorf("%s: sage_actions window %s", kind, c.args.Window)
			}
		}
	}
}

func TestPlanFor_M6SamplesWhatTheMatchersCompare(t *testing.T) {
	want := map[TriggerKind]map[probes.ID]int{
		TriggerCheckpoint: {probes.CheckpointActivity: 3},
		TriggerTempFiles: {probes.TempFileActivity: 2, probes.TempFileHolders: 2,
			probes.TempSpillStatements: 2},
		TriggerReplicationLag: {probes.ReplicationLag: 2, probes.StandbyReplayState: 2,
			probes.WALCheckpoint: 2},
		TriggerLWLock: {probes.LWLockWaits: 4},
	}
	for kind, counts := range want {
		plan, _ := planFor(kind, time.Hour)
		_, ids := planProbes(plan)
		for id, n := range counts {
			if ids[id] != n {
				t.Errorf("%s runs %s %d times, want %d", kind, id, ids[id], n)
			}
			if n > 1 && !seriesProbes[id] {
				t.Errorf("%s is sampled but not a series probe: samples would be dropped", id)
			}
		}
	}
}

// A checkpoint storm needs a longer window than one sample interval: a
// storm at the "too frequently" threshold requests one every 30 s.
func TestPlanFor_CheckpointWindowSpansSixSampleIntervals(t *testing.T) {
	plan, _ := planFor(TriggerCheckpoint, time.Hour)
	gaps := 0
	for _, st := range plan {
		if st.sample {
			gaps += st.waits()
		}
	}
	if gaps != 6 {
		t.Fatalf("checkpoint window = %d sample intervals, want 6", gaps)
	}
	lw, _ := planFor(TriggerLWLock, time.Hour)
	for _, st := range lw {
		if st.sample && st.waits() != 1 {
			t.Fatalf("lwlock steps wait %d intervals, want 1", st.waits())
		}
	}
	if (planStep{sample: true}).waits() != 1 || (planStep{sample: true, gap: 3}).waits() != 3 {
		t.Fatal("a sampling step waits one interval unless it names a gap")
	}
}

func TestDiagnose_M6FamiliesCarrySelfActionAndRefutations(t *testing.T) {
	fams := map[TriggerKind]causal.Family{TriggerCheckpoint: causal.FamilyCheckpoint,
		TriggerTempFiles: causal.FamilyTempFiles, TriggerReplicationLag: causal.FamilyReplicationLag,
		TriggerLWLock: causal.FamilyLWLock}
	for kind, fam := range fams {
		d := diagnose(Investigation{TriggerKind: kind}, nil)
		if d.Family != fam || d.Conclusive {
			t.Fatalf("%s: family %s conclusive %v", kind, d.Family, d.Conclusive)
		}
		c := conclusionOf(d)
		if c.State != StateInconclusive || len(c.Hypotheses) < 3 {
			t.Fatalf("%s conclusion = %+v", kind, c)
		}
		self := false
		for _, h := range c.Hypotheses {
			if h.RefutationProbe == "" {
				t.Errorf("%s hypothesis %s has no refutation probe (CHECK-37)", kind, h.Node)
			}
			self = self || h.Node == string(causal.SageOwnAction)
		}
		if !self {
			t.Errorf("%s has no pg_sage self-action hypothesis (CHECK-38)", kind)
		}
	}
}

func TestStartRequest_AcceptsM6Kinds(t *testing.T) {
	scope := Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()}
	for _, kind := range m6Kinds {
		req := StartRequest{Scope: scope, CaseID: "case", TriggerKind: kind}
		if err := req.Validate(); err != nil {
			t.Errorf("%s rejected: %v", kind, err)
		}
	}
	if string(TriggerCheckpoint) != string(causal.FamilyCheckpoint) ||
		string(TriggerTempFiles) != string(causal.FamilyTempFiles) ||
		string(TriggerReplicationLag) != string(causal.FamilyReplicationLag) ||
		string(TriggerLWLock) != string(causal.FamilyLWLock) {
		t.Fatal("trigger kinds and graph families must share names")
	}
}

// A model-proposed rerun of a sampled probe keeps every sample.
func TestCurrentObservations_KeepsM6Series(t *testing.T) {
	var obs []causal.Observation
	for _, id := range []probes.ID{probes.LWLockWaits, probes.LWLockWaits,
		probes.CheckpointActivity, probes.CheckpointActivity, probes.TempFileHolders,
		probes.TempFileHolders} {
		obs = append(obs, causal.Observation{EvidenceID: string(NewUUID()),
			Result: probes.Result{ProbeID: id}})
	}
	if got := currentObservations(obs); len(got) != len(obs) {
		t.Fatalf("kept %d of %d samples", len(got), len(obs))
	}
}
