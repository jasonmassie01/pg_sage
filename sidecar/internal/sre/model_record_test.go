package sre

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Persisted model output (Sage SRE M3): the ranking, narrative and
// proposed probe are stored beside the deterministic diagnosis, labeled,
// and validated again before any store I/O. The store refuses a ranking
// that would put another root above a concluded root cause.

func modelConclusion(t *testing.T) (Conclusion, []Evidence) {
	t.Helper()
	_, ev, d := idleChainFixture(t)
	c := conclusionOf(d)
	c.Summary.ModelRanking = &ModelRanking{Label: ModelRankingLabel, Basis: ModelRankingBasis,
		Nodes: idleRanking()}
	c.Summary.Narrative = &Narrative{Label: NarrativeLabel, Claims: []NarrativeClaim{{
		Text: "pid 4242 is idle in transaction.", EvidenceIDs: []UUID{ev[0].ID}}}}
	return c, ev
}

func TestConclusionModel_ValidPasses(t *testing.T) {
	c, ev := modelConclusion(t)
	if err := c.validate(); err != nil {
		t.Fatalf("valid model conclusion rejected: %v", err)
	}
	refs := map[UUID]bool{}
	for _, id := range c.evidenceRefs() {
		refs[id] = true
	}
	if !refs[ev[0].ID] {
		t.Fatalf("narrative evidence %s is not checked for scope: %v", ev[0].ID, refs)
	}
	c.Summary.ModelProbe = &ModelProbe{Label: ModelProbeLabel, ProbeID: "lock_graph",
		Rationale: "tells idle from active", EvidenceID: ev[1].ID}
	if err := c.validate(); err != nil {
		t.Fatalf("model probe rejected: %v", err)
	}
	found := false
	for _, id := range c.evidenceRefs() {
		found = found || id == ev[1].ID
	}
	if !found {
		t.Fatal("the model probe's evidence is not checked for scope")
	}
}

func TestConclusionModel_Negatives(t *testing.T) {
	cases := map[string]func(c *Conclusion){
		"other root first": func(c *Conclusion) {
			c.Summary.ModelRanking.Nodes = []string{"ddl_lock_queue", "idle_in_tx_holder"}
		},
		"ruled-out node": func(c *Conclusion) {
			c.Summary.ModelRanking.Nodes = append(idleRanking(), "hot_row_contention")
		},
		"unknown node": func(c *Conclusion) {
			c.Summary.ModelRanking.Nodes = []string{"idle_in_tx_holder", "cosmic_rays"}
		},
		"duplicate node": func(c *Conclusion) {
			c.Summary.ModelRanking.Nodes = []string{"idle_in_tx_holder", "idle_in_tx_holder"}
		},
		"ranking label":   func(c *Conclusion) { c.Summary.ModelRanking.Label = "confidence" },
		"narrative label": func(c *Conclusion) { c.Summary.Narrative.Label = "" },
		"no claims":       func(c *Conclusion) { c.Summary.Narrative.Claims = nil },
		"uncited claim":   func(c *Conclusion) { c.Summary.Narrative.Claims[0].EvidenceIDs = nil },
		"bad evidence id": func(c *Conclusion) {
			c.Summary.Narrative.Claims[0].EvidenceIDs = []UUID{"E1"}
		},
		"control character": func(c *Conclusion) {
			c.Summary.Narrative.Claims[0].Text = "idle\x07"
		},
		"too many claims": func(c *Conclusion) {
			claim := c.Summary.Narrative.Claims[0]
			for i := 0; i < MaxClaims; i++ {
				c.Summary.Narrative.Claims = append(c.Summary.Narrative.Claims, claim)
			}
		},
		"unknown probe": func(c *Conclusion) {
			c.Summary.ModelProbe = &ModelProbe{Label: ModelProbeLabel, ProbeID: "drop_table",
				Rationale: "x"}
		},
		"long rationale": func(c *Conclusion) {
			c.Summary.ModelProbe = &ModelProbe{Label: ModelProbeLabel, ProbeID: "lock_graph",
				Rationale: strings.Repeat("r", maxRationaleRunes+1)}
		},
	}
	for name, mutate := range cases {
		c, _ := modelConclusion(t)
		mutate(&c)
		if err := c.validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: validate = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// Without model output a conclusion validates exactly as before.
func TestConclusionModel_AbsentIsUnchanged(t *testing.T) {
	_, _, d := idleChainFixture(t)
	c := conclusionOf(d)
	if err := c.validate(); err != nil {
		t.Fatalf("deterministic conclusion rejected: %v", err)
	}
	if c.Summary.ModelRanking != nil || c.Summary.Narrative != nil ||
		c.Summary.ModelProbe != nil {
		t.Fatalf("deterministic summary carries model output: %+v", c.Summary)
	}
}

// After a model-proposed re-probe the newest observation of a one-shot
// probe is the one diagnosed; sampled probes keep their whole series.
func TestDiagnose_LatestOneShotObservationWins(t *testing.T) {
	inv, before, d0 := unknownLockFixture(t)
	if d0.Conclusive {
		t.Fatalf("fixture must start inconclusive: %+v", d0)
	}
	chain := idleChainRunner().Run(t.Context(), probes.LockGraph, probes.Args{})
	after := append(append([]Evidence(nil), before...), fixtureEvidence(t, chain)...)
	d := diagnoseEvidence(t, inv, after)
	if !d.Conclusive || d.Root == nil || string(d.Root.Node) != "idle_in_tx_holder" {
		t.Fatalf("diagnosis = %+v", d)
	}
	if d.Root.Support[0].EvidenceID != string(after[len(after)-1].ID) {
		t.Fatalf("root cites %s, want the re-probe %s", d.Root.Support[0].EvidenceID,
			after[len(after)-1].ID)
	}
	for _, m := range d.Missing {
		if m.ProbeID == probes.LockGraph {
			t.Fatalf("the superseded failure is still missing evidence: %+v", d.Missing)
		}
	}
	leak := leakRunner()
	ctx := t.Context()
	samples := fixtureEvidence(t, leak.Run(ctx, probes.ConnectionSaturation, probes.Args{}),
		leak.Run(ctx, probes.ConnectionSaturation, probes.Args{}),
		leak.Run(ctx, probes.ConnectionSaturation, probes.Args{}))
	conn := diagnoseEvidence(t, Investigation{TriggerKind: TriggerConnections}, samples)
	if conn.Root == nil || string(conn.Root.Node) != "connection_leak" {
		t.Fatalf("a three-sample series must still show the leak: %+v", conn)
	}
}

// The per-turn model timeout defaults within the wall budget and is
// validated like the other coordinator settings.
func TestCoordinatorConfig_ModelTimeout(t *testing.T) {
	cfg := DefaultCoordinatorConfig("test:timeout")
	if cfg.ModelTimeout <= 0 || cfg.ModelTimeout > CeilingActive {
		t.Fatalf("default model timeout = %s", cfg.ModelTimeout)
	}
	deps := CoordinatorDeps{Store: (*PostgresStore)(nil), Runner: newScriptedRunner()}
	for _, bad := range []time.Duration{0, -time.Second, CeilingActive + time.Second} {
		deps.Config = cfg
		deps.Config.ModelTimeout = bad
		if _, err := NewCoordinator(deps); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("model timeout %s = %v, want ErrInvalidRequest", bad, err)
		}
	}
	for _, good := range []time.Duration{time.Millisecond, CeilingActive} {
		deps.Config = cfg
		deps.Config.ModelTimeout = good
		if _, err := NewCoordinator(deps); err != nil {
			t.Errorf("model timeout %s rejected: %v", good, err)
		}
	}
}
