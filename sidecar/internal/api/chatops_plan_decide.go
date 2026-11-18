package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Plan cards from chat (plan-03b), on the approval-card rules: the
// callback is signed, processed once and attributed to a mapped operator
// or admin (callbackHandler); the token must name a plan card sent to this
// channel, unused and unexpired, offering the decision pressed, whose plan
// is still proposed at the hash the card showed. What chat may approve is
// checked again against the plan now (decision D7: risk tiers safe and
// moderate, never a never-automatic step), so a stale offer fails closed.
// The token is consumed before the decision and released when the plan
// store does not record it, so a card decides at most once and a failure
// costs no card. Approvals go through plan-03a's ApproveWith with via
// chat; every step still runs through the executor.

// chatPlanStores is the plan store a chat decision uses (tests swap it).
var chatPlanStores = databasePlanStore

// planCardTarget is a checked plan card with its database and plan now.
type planCardTarget struct {
	card   chatops.PlanCardDelivery
	inst   *fleet.DatabaseInstance
	detail changeplan.Detail
	// chatCap is notification_policy.chat_approval_max_tier (ux-09a).
	chatCap string
}

func (d *chatopsDeps) decidePlanCard(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, a chatops.Action, user auth.User) {
	t, ok := d.checkPlanCard(w, r, ch, a)
	if !ok || !d.allowChatDatabase(w, r, ch, a, user, t.card.Database) {
		return
	}
	decision, ok := chatPlanDecision(w, ch, a, t)
	if !ok {
		return
	}
	if _, err := d.planCards.Consume(r.Context(), a.PlanToken, user.ID, a.PlanChoice,
		a.MessageID); err != nil {
		writePlanCardError(w, r, err)
		return
	}
	store := chatPlanStores(t.inst)
	id := t.card.PlanID
	var err error
	if a.PlanChoice == chatops.PlanReject {
		err = store.Reject(r.Context(), id, fmt.Sprintf("rejected in %s by %s", a.Provider,
			user.Email), user.ID)
	} else {
		_, err = store.ApproveWith(r.Context(), id, decision, user.ID)
	}
	if err != nil {
		d.releasePlanCard(r, a, user)
		answerPlanFailure(w, r, ch, a, err)
		return
	}
	answerPlanDecision(w, ch, a, t, decision, user)
}

// checkPlanCard verifies the plan card a token names: issued to this
// channel, unused, unexpired, offering the decision, on a monitored
// database, its plan still proposed and unchanged since the card was sent.
func (d *chatopsDeps) checkPlanCard(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, a chatops.Action) (planCardTarget, bool) {
	t := planCardTarget{chatCap: d.chatCap}
	var err error
	if t.card, err = d.planCards.Lookup(r.Context(), a.PlanToken); err != nil {
		writePlanCardError(w, r, err)
		return t, false
	}
	if !checkPlanCardUse(w, r, ch, a, t.card) {
		return t, false
	}
	if t.inst = d.mgr.GetInstance(t.card.Database); t.inst == nil || t.inst.Pool == nil {
		sreErrorCode(w, "the card's database is not monitored", "not_found",
			http.StatusNotFound)
		return t, false
	}
	t.detail, err = chatPlanStores(t.inst).Detail(r.Context(), t.card.PlanID)
	switch {
	case errors.Is(err, changeplan.ErrNotFound):
		sreErrorCode(w, "the card's plan no longer exists", "not_found", http.StatusNotFound)
		return t, false
	case err != nil:
		answerPlanFailure(w, r, ch, a, err)
		return t, false
	case changeplan.PlanHash(t.detail.Plan) != t.card.PlanHash:
		slog.Warn("plan card refused: the plan changed after it was sent",
			"plan_id", t.card.PlanID, "card", t.card.ID)
		reply(ch, a, "Refused: this plan changed after the card was sent. Review the "+
			"current version in the pg_sage app.")
		sreErrorCode(w, "the plan changed after this card was sent", "content_changed",
			http.StatusConflict)
		return t, false
	case t.detail.Plan.Status != changeplan.StatusProposed:
		reply(ch, a, fmt.Sprintf("Not decided: plan #%d is already %s. Review it in the "+
			"pg_sage app.", t.card.PlanID, t.detail.Plan.Status))
		sreErrorCode(w, "the plan is no longer awaiting a decision", "not_pending",
			http.StatusConflict)
		return t, false
	}
	return t, true
}

// checkPlanCardUse refuses a card from another channel, a used or expired
// one, or a decision the card did not offer.
func checkPlanCardUse(w http.ResponseWriter, r *http.Request, ch notify.Channel,
	a chatops.Action, card chatops.PlanCardDelivery) bool {
	switch {
	case card.ChannelID != ch.ID:
		slog.Warn("plan card used from another channel", "card", card.ID,
			"issued_to", card.ChannelID, "used_from", ch.ID)
		sreErrorCode(w, "this card was sent to another channel", "wrong_channel",
			http.StatusForbidden)
	case card.UsedAt != nil:
		writePlanCardError(w, r, chatops.ErrCardUsed)
	case card.Expired(chatopsNow()):
		writePlanCardError(w, r, chatops.ErrCardExpired)
	case !card.Offers(a.PlanChoice):
		refuseNotOffered(w, ch, a, "this card does not offer that decision")
	default:
		return true
	}
	return false
}

// chatPlanDecision is the approval a choice records, when chat may make it
// on the plan now; reject records none.
func chatPlanDecision(w http.ResponseWriter, ch notify.Channel, a chatops.Action,
	t planCardTarget) (changeplan.Decision, bool) {
	offer := changeplan.OfferForChat(t.detail, t.chatCap)
	d := changeplan.Decision{PlanHash: t.card.PlanHash, Via: changeplan.ViaChat,
		ProposedOnly: true}
	allowed := true
	switch a.PlanChoice {
	case chatops.PlanReject:
		return d, true
	case chatops.PlanApproveAll:
		d.Mode, allowed = changeplan.ModeAll, offer.All
	case chatops.PlanApproveUpTo:
		d.Mode, d.UpTo = changeplan.ModeUpTo, t.card.UpTo
		allowed = offer.UpTo > 0 && offer.UpTo == t.card.UpTo
	case chatops.PlanApproveNext:
		d.Mode, d.UpTo, allowed = changeplan.ModeUpTo, 1, offer.Next
	default:
		allowed = false
	}
	if !allowed {
		why := offer.Blocked
		if why == "" {
			why = "the plan no longer allows that decision from chat"
		}
		refuseNotOffered(w, ch, a, why)
		return d, false
	}
	return d, true
}

func refuseNotOffered(w http.ResponseWriter, ch notify.Channel, a chatops.Action,
	why string) {
	reply(ch, a, "Not approved: "+why+"; that needs the pg_sage app. The card stays "+
		"usable for what it offers.")
	sreErrorCode(w, "not offered from chat: "+why, "not_offered", http.StatusConflict)
}
