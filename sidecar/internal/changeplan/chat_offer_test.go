package changeplan

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// plan-03b, decision D7 (the recommended default): from chat a person may
// approve steps of risk tier safe and moderate. A plan whose first step
// outside that set (or a never-automatic step) is at ordinal K offers
// "approve up to step K-1"; at ordinal 1 it offers no approval at all.
// No concurrent access test: OfferForChat and StepRiskTier are pure.

func analyzeStep(ordinal int) Step {
	return Step{Ordinal: ordinal, Kind: KindAction, ActionType: "analyze_table",
		ProposedSQL: "ANALYZE public.orders", Metric: MetricReadback}
}

func terminateStep(ordinal int) Step {
	return Step{Ordinal: ordinal, Kind: KindAction, ActionType: "terminate_backend",
		ProposedSQL: "SELECT pg_terminate_backend(4242)", Metric: MetricReadback}
}

func offerDetail(steps ...Step) Detail {
	p := Plan{ID: 9, Status: StatusProposed, Steps: steps}
	cov := make([]StepCoverage, 0, len(steps))
	for i := range steps {
		p.Steps[i].ID = int64(90 + i)
		cov = append(cov, StepCoverage{StepID: p.Steps[i].ID, Ordinal: steps[i].Ordinal})
	}
	return Detail{Plan: p, Coverage: cov}
}

func moderate(ordinal int) Step {
	return indexStep(ordinal, "idx_"+string(rune('a'+ordinal)), "c", 11, 101)
}

func TestStepRiskTier_ReadsTheExecutorContracts(t *testing.T) {
	cases := map[string]struct {
		step Step
		want policy.RiskTier
	}{
		"analyze is safe":          {analyzeStep(1), policy.RiskSafe},
		"index create is moderate": {moderate(1), policy.RiskModerate},
		"terminate is high":        {terminateStep(1), policy.RiskHigh},
		"check changes nothing":    {Step{Ordinal: 1, Kind: KindCheck}, policy.RiskSafe},
		"operator runs it":         {Step{Ordinal: 1, Kind: KindOperator}, policy.RiskSafe},
		"unknown type fails closed": {Step{Ordinal: 1, Kind: KindAction, ActionType: "x"},
			policy.RiskHigh},
		"empty action fails closed": {Step{Ordinal: 1, Kind: KindAction}, policy.RiskHigh},
		"SQL higher than its type wins": {Step{Ordinal: 1, Kind: KindAction,
			ActionType: "analyze_table", ProposedSQL: "SELECT pg_terminate_backend(1)"},
			policy.RiskHigh},
		"unknown kind fails closed": {Step{Ordinal: 1, Kind: "mystery"}, policy.RiskHigh},
	}
	for name, c := range cases {
		if got := StepRiskTier(c.step); got != c.want {
			t.Errorf("%s: tier %q, want %q", name, got, c.want)
		}
	}
}

func TestOfferForChat_ThreeModerateStepsOfferAllAndStepOne(t *testing.T) {
	o := OfferForChat(offerDetail(moderate(1), moderate(2), moderate(3)))
	if !o.All || !o.Next || o.UpTo != 0 || o.BlockedAt != 0 || o.Blocked != "" {
		t.Fatalf("offer %+v, want all and step 1 only, no up-to, nothing blocked", o)
	}
}

func TestOfferForChat_HighTierBoundaries(t *testing.T) {
	cases := map[string]struct {
		d               Detail
		all, next       bool
		upTo, blockedAt int
	}{
		"high at 1: no approval": {offerDetail(terminateStep(1), moderate(2)),
			false, false, 0, 1},
		"high at 2: step 1 only": {offerDetail(moderate(1), terminateStep(2), moderate(3)),
			false, true, 0, 2},
		"high at 3: up to 2": {offerDetail(moderate(1), analyzeStep(2), terminateStep(3)),
			false, true, 2, 3},
		"high at 5: up to 4": {offerDetail(moderate(1), moderate(2), moderate(3),
			analyzeStep(4), terminateStep(5)), false, true, 4, 5},
		"single safe step: all only": {offerDetail(analyzeStep(1)), true, false, 0, 0},
		"single high step":           {offerDetail(terminateStep(1)), false, false, 0, 1},
	}
	for name, c := range cases {
		o := OfferForChat(c.d)
		if o.All != c.all || o.Next != c.next || o.UpTo != c.upTo ||
			o.BlockedAt != c.blockedAt {
			t.Errorf("%s: offer %+v, want all=%v next=%v up_to=%d blocked_at=%d", name, o,
				c.all, c.next, c.upTo, c.blockedAt)
		}
		if c.blockedAt > 0 && o.Blocked == "" {
			t.Errorf("%s: a blocked plan says nothing about why", name)
		}
	}
}

func TestOfferForChat_NeverAutomaticStepBlocksLikeHighTier(t *testing.T) {
	d := offerDetail(moderate(1), moderate(2), moderate(3))
	d.Coverage[2].NeedsOwnApproval = true
	d.Coverage[2].Reason = "rollback_missing: no rollback"
	o := OfferForChat(d)
	if o.All || !o.Next || o.UpTo != 2 || o.BlockedAt != 3 {
		t.Fatalf("offer %+v, want up to 2 and step 1 only, blocked at 3", o)
	}
}

func TestOfferForChat_CheckAndOperatorStepsNeverBlock(t *testing.T) {
	o := OfferForChat(offerDetail(moderate(1), Step{Ordinal: 2, Kind: KindCheck},
		Step{Ordinal: 3, Kind: KindOperator}))
	if !o.All || o.BlockedAt != 0 {
		t.Fatalf("offer %+v, want all: check and operator steps change nothing", o)
	}
}

func TestOfferForChat_NilEmptyAndMissingCoverage(t *testing.T) {
	if o := OfferForChat(Detail{}); o.All || o.Next || o.UpTo != 0 {
		t.Fatalf("an empty plan offers %+v, want nothing", o)
	}
	d := offerDetail(moderate(1), moderate(2), moderate(3))
	d.Coverage = d.Coverage[:1] // steps 2 and 3 have no coverage row
	if o := OfferForChat(d); o.All || o.BlockedAt != 2 || !o.Next {
		t.Fatalf("missing coverage offers %+v, want blocked at 2 (fail closed)", o)
	}
	d = offerDetail(moderate(2), moderate(1)) // out of order in the slice
	if o := OfferForChat(d); !o.All || !o.Next {
		t.Fatalf("steps out of slice order offer %+v, want all", o)
	}
}

func TestChatApprovableTiers_AreSafeAndModerate(t *testing.T) {
	if len(ChatApprovableTiers) != 2 || !ChatApprovable(policy.RiskSafe) ||
		!ChatApprovable(policy.RiskModerate) || ChatApprovable(policy.RiskHigh) ||
		ChatApprovable("") {
		t.Fatalf("chat tiers %v, want safe and moderate (D7)", ChatApprovableTiers)
	}
}
