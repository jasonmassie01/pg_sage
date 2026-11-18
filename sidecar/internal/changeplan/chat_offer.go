package changeplan

import (
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// What a person may approve from chat (plan-03b, decision D7). The
// recommended default is built: steps of risk tier safe and moderate may be
// approved from a chat card. A plan whose first step outside those tiers,
// or first never-automatic step (NeedsOwnApproval), is at ordinal K offers
// "approve up to step K-1" and says the rest needs the app; at ordinal 1 a
// chat card offers no approval at all. Reject is always offered.

// ChatApprovableTiers are the risk tiers a chat card may approve (D7). It
// is the one place the owner's decision changes.
var ChatApprovableTiers = []policy.RiskTier{policy.RiskSafe, policy.RiskModerate}

// ChatApprovable reports whether a step of tier may be approved from chat.
func ChatApprovable(tier policy.RiskTier) bool {
	return tier != "" && slices.Contains(ChatApprovableTiers, tier)
}

// tierRank orders the risk tiers; an unknown tier ranks above high.
var tierRank = map[policy.RiskTier]int{policy.RiskSafe: 1, policy.RiskModerate: 2,
	policy.RiskHigh: 3}

// StepRiskTier is step s's risk tier: the higher base tier of the executor
// contracts it runs under (its action type and the one its SQL classifies
// as). An action step without a contract is high (fail closed). Check and
// operator steps change nothing through pg_sage, so they are safe.
func StepRiskTier(s Step) policy.RiskTier {
	switch s.Kind {
	case KindCheck, KindOperator:
		return policy.RiskSafe
	case KindAction:
	default:
		return policy.RiskHigh
	}
	tier := policy.RiskTier("")
	types := []string{s.ActionType, executor.ActionTypeForSQL(s.ProposedSQL)}
	for _, actionType := range types {
		contract, ok := executor.ContractForActionType(actionType)
		if !ok {
			return policy.RiskHigh
		}
		t := policy.RiskTier(contract.BaseRiskTier)
		rank, known := tierRank[t]
		if !known {
			return policy.RiskHigh
		}
		if rank > tierRank[tier] {
			tier = t
		}
	}
	return tier
}

// ChatOffer is what a chat card of a plan offers besides reject: approve
// every step, approve step 1 only, or approve up to step UpTo. BlockedAt is
// the first step chat cannot approve (0: none) and Blocked says why.
type ChatOffer struct {
	All       bool
	Next      bool
	UpTo      int
	BlockedAt int
	Blocked   string
}

// OfferForChat is the chat offer for plan detail d. A step whose coverage
// row is missing counts as blocked (fail closed).
func OfferForChat(d Detail) ChatOffer {
	steps := slices.Clone(d.Plan.Steps)
	slices.SortFunc(steps, func(a, b Step) int { return a.Ordinal - b.Ordinal })
	if len(steps) == 0 {
		return ChatOffer{}
	}
	needs := map[int64]StepCoverage{}
	for _, c := range d.Coverage {
		needs[c.StepID] = c
	}
	var o ChatOffer
	for _, s := range steps {
		if why := chatBlock(s, needs); why != "" {
			o.BlockedAt, o.Blocked = s.Ordinal, why
			break
		}
	}
	switch {
	case o.BlockedAt == 0:
		o.All, o.Next = true, len(steps) > 1
	case o.BlockedAt == steps[0].Ordinal:
	default:
		o.Next = true
		if last := o.BlockedAt - 1; last > steps[0].Ordinal {
			o.UpTo = last
		}
	}
	return o
}

// chatBlock is why step s cannot be approved from chat ("" when it can).
func chatBlock(s Step, coverage map[int64]StepCoverage) string {
	c, ok := coverage[s.ID]
	switch {
	case !ok:
		return fmt.Sprintf("step %d has no approval coverage to check", s.Ordinal)
	case c.NeedsOwnApproval:
		return fmt.Sprintf("step %d is never automatic and needs its own approval",
			s.Ordinal)
	}
	if tier := StepRiskTier(s); !ChatApprovable(tier) {
		return fmt.Sprintf("step %d is risk tier %s", s.Ordinal, tier)
	}
	return ""
}
