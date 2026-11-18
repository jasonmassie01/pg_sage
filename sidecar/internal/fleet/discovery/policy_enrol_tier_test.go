package discovery

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01d: a set enrolls at its enrollment.tier; agent-created
// databases are observe-only by default and enroll past max_enrolled
// (they count against the 2,000 observe-only members, not the 50).
// No concurrent access tests: the runner's cycle is single-threaded and
// the claim's lock between sidecars is tested on the real store.

func (m *fakeMembers) tiers() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for _, r := range m.rows {
		out[r.ExternalID] = r.Tier
	}
	return out
}

func TestPolicyEnrolTier_AgentCreatedIsObserveOnlyByDefault(t *testing.T) {
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:builder"), cand("b")},
		defaultSet(), agentSet())
	rep := e.r.Cycle(context.Background())
	if rep.Enrolled != 2 || len(rep.Errors) != 0 {
		t.Fatalf("report = %+v", rep)
	}
	got := e.members.tiers()
	if got["a"] != store.TierObserveOnly || got["b"] != store.TierFull {
		t.Fatalf("tiers %v, want a observe_only and b full", got)
	}
}

func TestPolicyEnrolTier_ExplicitTierWins(t *testing.T) {
	agents := agentSet()
	agents.Requirements.Enrollment.Tier = policy.EnrollTierFull
	def := defaultSet()
	def.Requirements.Enrollment.Tier = policy.EnrollTierObserveOnly
	e := newSetsEnv(t, []Candidate{agentCand("a", "agent:x"), cand("b")}, def, agents)
	e.r.Cycle(context.Background())
	got := e.members.tiers()
	if got["a"] != store.TierFull || got["b"] != store.TierObserveOnly {
		t.Fatalf("tiers %v, want a full and b observe_only", got)
	}
}

// Boundary: max_enrolled 2 holds full members to 2, while an observe-only
// set keeps enrolling past it, per_cycle at a time.
func TestPolicyEnrolTier_ObserveOnlyEnrollsPastMaxEnrolled(t *testing.T) {
	var cands []Candidate
	for i := range 8 {
		cands = append(cands, agentCand(fmt.Sprintf("a%d", i), "agent:x"))
	}
	for i := range 3 {
		cands = append(cands, cand(fmt.Sprintf("p%d", i)))
	}
	e := newSetsEnv(t, cands, defaultSet(), agentSet())
	e.r.cfg.MaxEnrolled = 2
	total := 0
	for range 3 {
		total += e.r.Cycle(context.Background()).Enrolled
		e.advance(10 * time.Minute)
	}
	full, observe := 0, 0
	for _, tier := range e.members.tiers() {
		switch tier {
		case store.TierFull:
			full++
		case store.TierObserveOnly:
			observe++
		}
	}
	if full != 2 || observe != 8 || total != 10 {
		t.Fatalf("%d full, %d observe-only (%d enrolled), want 2 and 8", full, observe,
			total)
	}
}
