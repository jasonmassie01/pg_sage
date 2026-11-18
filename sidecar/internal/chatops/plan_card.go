package chatops

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Plan cards (plan-03b) reuse the approval-card token mechanism: each
// button carries one plan decision and an opaque 22-character token naming
// one plan card sent to one channel; the server keeps only the token's
// SHA-256. Who decided comes from the provider envelope. Which decisions a
// card offers is fixed when it is sent (always reject; the approvals the
// plan's risk tiers allow from chat) and checked again when it is used.

// PlanChoice is a plan card decision (sage.plan_card_deliveries.decision).
type PlanChoice string

// The plan card decisions: approve every step, approve up to the card's
// step K, approve step 1 only, or reject the plan.
const (
	PlanApproveAll  PlanChoice = "approve_all"
	PlanApproveUpTo PlanChoice = "approve_up_to"
	PlanApproveNext PlanChoice = "approve_next"
	PlanReject      PlanChoice = "reject"
)

// Valid reports whether c is one of the plan card decisions.
func (c PlanChoice) Valid() bool {
	switch c {
	case PlanApproveAll, PlanApproveUpTo, PlanApproveNext, PlanReject:
		return true
	}
	return false
}

// Slack button action ids of a plan card.
const (
	SlackPlanApproveAllAction  = "sage_plan_all"
	SlackPlanApproveUpToAction = "sage_plan_up_to"
	SlackPlanApproveNextAction = "sage_plan_next"
	SlackPlanRejectAction      = "sage_plan_reject"
)

// ErrChoiceNotOffered refuses a decision the plan card did not offer.
var ErrChoiceNotOffered = errors.New("chatops: the plan card does not offer this decision")

var slackPlanActions = map[string]PlanChoice{
	SlackPlanApproveAllAction:  PlanApproveAll,
	SlackPlanApproveUpToAction: PlanApproveUpTo,
	SlackPlanApproveNextAction: PlanApproveNext,
	SlackPlanRejectAction:      PlanReject,
}

// planDataPrefix starts a plan card's Telegram callback data:
// "sage:p<verb>:<token>", verb a (all), u (up to), n (step 1 only) or r
// (reject). At most 30 bytes, within Telegram's 64.
const planDataPrefix = "sage:p"

var planVerbs = map[PlanChoice]byte{PlanApproveAll: 'a', PlanApproveUpTo: 'u',
	PlanApproveNext: 'n', PlanReject: 'r'}

// PlanCallbackData is the Telegram button payload of a plan decision.
func PlanCallbackData(c PlanChoice, token string) string {
	verb, ok := planVerbs[c]
	if !ok {
		verb = 'r' // an unknown decision can only ever reject
	}
	return planDataPrefix + string(verb) + ":" + token
}

// setPlan records a plan decision and its token on the action.
func (a *Action) setPlan(c PlanChoice, token string) error {
	if !ValidCardToken(token) {
		return fmt.Errorf("%w: bad plan card token", ErrMalformed)
	}
	a.PlanChoice, a.PlanToken, a.Decision = c, token, DecisionApprove
	if c == PlanReject {
		a.Decision = DecisionDeny
	}
	return nil
}

// setPlanData reads plan card callback data into the action.
func (a *Action) setPlanData(data string) error {
	rest := strings.TrimPrefix(data, planDataPrefix)
	if len(rest) < 2 || rest[1] != ':' {
		return fmt.Errorf("%w: unknown plan callback data", ErrMalformed)
	}
	for c, verb := range planVerbs {
		if rest[0] == verb {
			return a.setPlan(c, rest[2:])
		}
	}
	return fmt.Errorf("%w: unknown plan decision", ErrMalformed)
}

// Offers reports whether the card offered decision c.
func (d PlanCardDelivery) Offers(c PlanChoice) bool {
	return c.Valid() && slices.Contains(d.Offered, c)
}
