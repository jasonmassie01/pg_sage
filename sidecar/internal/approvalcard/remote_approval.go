package approvalcard

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// What a person may approve from chat (ux-09a, decision D7). The cap is
// notification_policy.chat_approval_max_tier: safe, moderate (the
// default, also what an empty cap means) or none. A high-risk change is
// approved in pg_sage, never from chat, whatever the cap says, and an
// unknown tier or cap fails closed. Reject and Snooze are never capped.

// The caps, lowest first; they mirror config.ChatApprovalCap*.
const (
	capNone     = "none"
	capSafe     = "safe"
	capModerate = "moderate"
)

// remoteTierRank orders the tiers chat may ever approve.
var remoteTierRank = map[policy.RiskTier]int{policy.RiskSafe: 1, policy.RiskModerate: 2}

// remoteCapRank is the highest tier rank each cap allows.
var remoteCapRank = map[string]int{capNone: 0, capSafe: 1, capModerate: 2, "": 2}

// RemoteApprovalAllowed reports whether a change of tier may be approved
// from chat under cap; when it may not, reason says why and names the tier
// (or the cap, when the cap is not one pg_sage knows).
func RemoteApprovalAllowed(tier policy.RiskTier, cap string) (bool, string) {
	if tier == policy.RiskHigh {
		return false, "risk tier high is approved in pg_sage only"
	}
	limit, ok := remoteCapRank[cap]
	if !ok {
		return false, fmt.Sprintf("chat approval cap %q is not safe, moderate or none", cap)
	}
	rank, known := remoteTierRank[tier]
	if !known {
		return false, fmt.Sprintf("risk tier %q is unknown", tier)
	}
	if rank > limit {
		shown := cap
		if shown == "" {
			shown = capModerate
		}
		return false, fmt.Sprintf("risk tier %s is above the chat approval cap %s", tier,
			shown)
	}
	return true, ""
}

// RemoteApprovalRefusal is the chat reply to an Approve above the cap: the
// reason and where in pg_sage the item is decided.
func RemoteApprovalRefusal(reason string, queueID int) string {
	return fmt.Sprintf("High-risk changes are approved in pg_sage: %s (%s). The card "+
		"still takes Reject.", remoteApprovalLink(queueID), reason)
}

// remoteApprovalLink is the inbox entry of a queue item.
func remoteApprovalLink(queueID int) string {
	return fmt.Sprintf("#/inbox?kind=change_approval&queue=%d", queueID)
}

// ActionTier is a queue item's risk tier for a remote approval: the highest
// of its stored risk and the base tier of every executor contract it runs
// under (its action type and the one its SQL classifies as). Anything
// unknown is high (fail closed).
func ActionTier(a store.QueuedAction) policy.RiskTier {
	ranks := map[policy.RiskTier]int{policy.RiskSafe: 1, policy.RiskModerate: 2,
		policy.RiskHigh: 3}
	tier := policy.RiskTier(strings.TrimSpace(a.ActionRisk))
	if _, ok := ranks[tier]; !ok {
		return policy.RiskHigh
	}
	types := []string{executor.ActionTypeForSQL(a.ProposedSQL)}
	if t := strings.TrimSpace(a.ActionType); t != "" {
		types = append(types, t)
	}
	for _, actionType := range types {
		contract, ok := executor.ContractForActionType(actionType)
		if !ok {
			return policy.RiskHigh
		}
		t := policy.RiskTier(contract.BaseRiskTier)
		rank, known := ranks[t]
		if !known {
			return policy.RiskHigh
		}
		if rank > ranks[tier] {
			tier = t
		}
	}
	return tier
}

// CardRefOfCapped is the card's chat reference under the chat approval
// cap: above it the card carries no Approve button and says where in
// pg_sage it is approved.
func CardRefOfCapped(c Card, cap string) notify.CardRef {
	ref := CardRefOf(c)
	if ok, reason := RemoteApprovalAllowed(policy.RiskTier(c.Risk.Tier), cap); !ok {
		ref.NoApprove, ref.ApproveIn = true, RemoteApprovalRefusal(reason, c.QueueID)
	}
	return ref
}
