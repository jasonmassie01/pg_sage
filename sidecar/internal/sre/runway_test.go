package sre

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Pre-incident (runway) investigations (AI-SRE-SPEC §4 R2): the three
// runway trigger kinds, their fixed probe plans within the 12-probe
// ceiling, the family matcher each dispatches to, and the custodian
// proposals a conclusion may carry.

func TestRunwayKinds_AreValidTriggers(t *testing.T) {
	scope := Scope{DeploymentID: NewUUID(), DatabaseID: NewUUID()}
	for _, k := range []TriggerKind{TriggerWraparound, TriggerDiskWAL, TriggerSequence} {
		req := StartRequest{Scope: scope, CaseID: "finding:db:forecast:x", TriggerKind: k,
			Subject: "xid"}
		if err := req.Validate(); err != nil {
			t.Errorf("%s: %v", k, err)
		}
		if !k.Runway() {
			t.Errorf("%s is not reported as a runway kind", k)
		}
	}
	for _, k := range []TriggerKind{TriggerLock, TriggerWAL, TriggerPlan, TriggerOperator} {
		if k.Runway() {
			t.Errorf("%s reported as a runway kind", k)
		}
	}
}

func planIDs(steps []planStep) (all []probes.ID, sampled []probes.ID) {
	for _, st := range steps {
		for _, c := range st.calls {
			all = append(all, c.id)
			if st.sample {
				sampled = append(sampled, c.id)
			}
		}
	}
	return all, sampled
}

func TestRunwayPlans_SampleTwiceWithinTheCeiling(t *testing.T) {
	want := map[TriggerKind][]probes.ID{
		TriggerWraparound: {probes.XIDRunwayProbe},
		TriggerDiskWAL: {probes.ReplicationSlots, probes.WALCheckpoint,
			probes.Archiver},
		TriggerSequence: {probes.SequenceRunwayProbe},
	}
	for kind, series := range want {
		steps, ok := runwayPlan(kind, time.Hour, 6*time.Hour)
		if !ok || len(steps) != 2 || !steps[1].sample || steps[0].sample {
			t.Fatalf("%s plan = %+v (%v), want a step then a sampling step", kind, steps, ok)
		}
		all, sampled := planIDs(steps)
		if len(all) > DefaultLimits().MaxProbes-2 {
			t.Errorf("%s runs %d probes; it must leave two for the model", kind, len(all))
		}
		if strings.Join(idStrings(sampled), ",") != strings.Join(idStrings(series), ",") {
			t.Errorf("%s samples %v twice, want %v", kind, sampled, series)
		}
		var trends, actions bool
		for _, c := range steps[0].calls {
			switch c.id {
			case probes.RunwayTrendsProbe:
				trends = c.args.Window == 6*time.Hour
			case probes.SageActions:
				actions = c.args.Window == time.Hour
			}
		}
		if !trends || !actions {
			t.Errorf("%s: trends over the runway window %v, own actions over the action "+
				"window %v", kind, trends, actions)
		}
		for _, id := range all {
			spec, _ := probes.Catalog().Spec(id)
			if err := probes.Catalog().CheckArgs(id, argsOf(steps, id)); err != nil ||
				spec.ID == "" {
				t.Errorf("%s: %s is not a valid catalog call: %v", kind, id, err)
			}
		}
	}
	if _, ok := runwayPlan(TriggerLock, time.Hour, time.Hour); ok {
		t.Fatal("a lock trigger got a runway plan")
	}
}

func argsOf(steps []planStep, id probes.ID) probes.Args {
	for _, st := range steps {
		for _, c := range st.calls {
			if c.id == id {
				return c.args
			}
		}
	}
	return probes.Args{}
}

func idStrings(ids []probes.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

// A zero runway window is the documented default; the coordinator's plan
// uses it for runway kinds and the R1 plans for the others.
func TestCoordinatorPlan_RunwayWindowDefault(t *testing.T) {
	c := &Coordinator{cfg: DefaultCoordinatorConfig("k")}
	c.cfg.RunwayWindow = 0
	steps, ok := c.plan(TriggerSequence)
	if !ok || argsOf(steps, probes.RunwayTrendsProbe).Window != DefaultRunwayWindow {
		t.Fatalf("sequence plan = %+v, want trends over %s", steps, DefaultRunwayWindow)
	}
	lock, ok := c.plan(TriggerLock)
	if !ok || len(lock) != 1 {
		t.Fatalf("lock plan = %+v", lock)
	}
}

func TestCoordinatorConfig_RunwayWindowBounds(t *testing.T) {
	for _, c := range []struct {
		w  time.Duration
		ok bool
	}{{0, true}, {time.Minute, true}, {probes.MaxWindow, true},
		{time.Minute - time.Second, false}, {probes.MaxWindow + time.Second, false}} {
		cfg := DefaultCoordinatorConfig("k")
		cfg.RunwayWindow = c.w
		if err := cfg.validate(false); (err == nil) != c.ok {
			t.Errorf("runway window %s: err = %v, want ok=%v", c.w, err, c.ok)
		}
	}
}

func TestDiagnose_DispatchesRunwayKinds(t *testing.T) {
	for kind, fam := range map[TriggerKind]causal.Family{
		TriggerWraparound: causal.FamilyWraparound, TriggerDiskWAL: causal.FamilyDiskWAL,
		TriggerSequence: causal.FamilySequence} {
		d := diagnose(Investigation{TriggerKind: kind, Subject: "sequence public.s"}, nil)
		if d.Family != fam {
			t.Errorf("%s diagnosed as %s, want %s", kind, d.Family, fam)
		}
		var selfAction bool
		for _, m := range d.Missing {
			selfAction = selfAction || m.ProbeID == probes.SageActions
		}
		if !selfAction {
			t.Errorf("%s: pg_sage's own actions were not asked about: %+v", kind, d.Missing)
		}
	}
}

func validProposal() ActionProposal {
	return ActionProposal{Feature: "freeze", Action: "VACUUM (FREEZE) public.events",
		SQL: `VACUUM (FREEZE) "public"."events"`, Targets: []string{"public.events"},
		Verdict: "queue_for_approval", RiskTier: "safe"}
}

func TestConclusion_ValidatesProposals(t *testing.T) {
	base := Conclusion{State: StateInconclusive, Summary: Summary{Reason: "x"}}
	ok := base
	ok.Summary.Proposals = []ActionProposal{validProposal()}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid proposal refused: %v", err)
	}
	long := validProposal()
	long.Action = strings.Repeat("a", 513)
	noVerdict := validProposal()
	noVerdict.Verdict = ""
	tooMany := []ActionProposal{validProposal(), validProposal(), validProposal(),
		validProposal()}
	manyTargets := validProposal()
	manyTargets.Targets = []string{"a", "b", "c", "d", "e"}
	for name, ps := range map[string][]ActionProposal{"long action": {long},
		"no verdict": {noVerdict}, "four proposals": tooMany,
		"five targets": {manyTargets}} {
		c := base
		c.Summary.Proposals = ps
		if err := c.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
