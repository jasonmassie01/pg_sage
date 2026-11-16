package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
)

// The answers to a plan card decision (plan-03b): the chat reply and the
// callback's JSON.

// couldNotRecord is the chat reply when the plan store failed.
const couldNotRecord = "pg_sage could not record the decision; the card stays usable. " +
	"Try again, or decide in the pg_sage app."

func writePlanCardError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, chatops.ErrChoiceNotOffered) {
		sreErrorCode(w, "this card does not offer that decision", "not_offered",
			http.StatusConflict)
		return
	}
	writeCardError(w, r, err)
}

// releasePlanCard frees the card user consumed when its decision was not
// recorded. A failed release leaves the card used (fail closed): the plan
// can still be decided in the app.
func (d *chatopsDeps) releasePlanCard(r *http.Request, a chatops.Action, user auth.User) {
	if err := d.planCards.Release(r.Context(), a.PlanToken, user.ID); err != nil {
		slog.Error("plan card not released after an unrecorded decision; decide the "+
			"plan in the app", "user_id", user.ID, "error", err)
	}
}

// answerPlanFailure answers a decision the plan store did not record: a
// changed plan, a refusal with its code, or a store failure (logged, with
// nothing internal in the reply or the body).
func answerPlanFailure(w http.ResponseWriter, r *http.Request, ch notify.Channel,
	a chatops.Action, err error) {
	var de *changeplan.DecisionError
	var ce *changeplan.CapError
	var ip *changeplan.InvalidPlanError
	switch {
	case errors.Is(err, changeplan.ErrStaleHash), errors.Is(err, changeplan.ErrHashMismatch):
		reply(ch, a, "Refused: this plan changed after the card was sent. Review the "+
			"current version in the pg_sage app.")
	case errors.As(err, &de), errors.As(err, &ce), errors.As(err, &ip),
		errors.Is(err, changeplan.ErrNotApprovable), errors.Is(err, changeplan.ErrNotRejectable),
		errors.Is(err, changeplan.ErrNotFound):
		reply(ch, a, "Not decided: pg_sage refused the decision. Review the plan in the "+
			"pg_sage app.")
	default:
		reply(ch, a, couldNotRecord)
	}
	writePlanError(w, r, "chat plan decision", err)
}

// answerPlanDecision replies with the recorded decision and answers the
// callback.
func answerPlanDecision(w http.ResponseWriter, ch notify.Channel, a chatops.Action,
	t planCardTarget, d changeplan.Decision, user auth.User) {
	id, db, steps := t.card.PlanID, t.card.Database, len(t.detail.Plan.Steps)
	body := map[string]any{"ok": true, "database": db, "plan_id": id,
		"decision": a.PlanChoice, "status": changeplan.StatusApproved}
	var text string
	switch a.PlanChoice {
	case chatops.PlanReject:
		body["status"] = changeplan.StatusRejected
		text = fmt.Sprintf("Rejected by %s: plan #%d on %s. pg_sage will not run it.",
			user.Email, id, db)
	case chatops.PlanApproveAll:
		text = fmt.Sprintf("Approved by %s: all %d steps of plan #%d on %s. Each step runs "+
			"through the executor and is verified before the next.", user.Email, steps, id, db)
	default:
		body["up_to"] = d.UpTo
		text = fmt.Sprintf("Approved by %s up to step %d of plan #%d on %s.", user.Email,
			d.UpTo, id, db)
		if offer := changeplan.OfferForChat(t.detail); offer.BlockedAt > 0 {
			text += fmt.Sprintf(" Step %d onward needs the pg_sage app: %s.",
				offer.BlockedAt, offer.Blocked)
		} else {
			text += " Approve the rest in the pg_sage app."
		}
	}
	reply(ch, a, text)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("chat plan card response not written", "plan_id", id, "error", err)
	}
}
