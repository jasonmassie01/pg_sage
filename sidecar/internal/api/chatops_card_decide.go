package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// Approval-card decisions from chat (roadmap 1.5). The callback is
// already signed by its provider, processed once per delivery, and
// attributed to a mapped operator or admin (callbackHandler). The card
// token must then name a card sent to this very channel that is unused
// and unexpired, and the queue item must still have the content the card
// showed. The token is consumed before the decision, so a card decides at
// most once; the decision goes through the UI's approval path.

// chatSnooze is how long a chat Snooze defers a card.
const chatSnooze = 4 * time.Hour

// cardOutcome is a card decision's response, chat reply and HTTP status.
type cardOutcome struct {
	body   map[string]any
	text   string
	status int
}

// decideCard applies one card decision.
func (d *chatopsDeps) decideCard(w http.ResponseWriter, r *http.Request, ch notify.Channel,
	a chatops.Action, user auth.User) {
	card, inst, current, ok := d.checkCard(w, r, ch, a)
	if !ok {
		return
	}
	if _, err := d.cards.Consume(r.Context(), a.CardToken, user.ID, a.Decision,
		a.MessageID); err != nil {
		writeCardError(w, r, err)
		return
	}
	as := store.NewActionStore(inst.Pool)
	var out cardOutcome
	switch a.Decision {
	case chatops.DecisionApprove:
		out = approveCardFromChat(r, as, inst, card, current, user)
	case chatops.DecisionSnooze:
		out = snoozeCardFromChat(r, as, card, a, user)
	default:
		out = rejectCardFromChat(r, as, card, a, user)
	}
	out.body["database"], out.body["queue_id"] = inst.Name, card.QueueID
	reply(ch, a, out.text)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.status)
	if err := json.NewEncoder(w).Encode(out.body); err != nil {
		slog.Warn("chat card response not written", "queue_id", card.QueueID, "error", err)
	}
}

// checkCard verifies the card a token names: issued to this channel,
// unused, unexpired, and unchanged since it was sent.
func (d *chatopsDeps) checkCard(w http.ResponseWriter, r *http.Request, ch notify.Channel,
	a chatops.Action) (chatops.CardDelivery, *fleet.DatabaseInstance, *store.QueuedAction,
	bool) {
	card, err := d.cards.Lookup(r.Context(), a.CardToken)
	switch {
	case err != nil:
		writeCardError(w, r, err)
		return card, nil, nil, false
	case card.ChannelID != ch.ID:
		slog.Warn("approval card used from another channel", "card", card.ID,
			"issued_to", card.ChannelID, "used_from", ch.ID)
		sreErrorCode(w, "this card was sent to another channel", "wrong_channel",
			http.StatusForbidden)
		return card, nil, nil, false
	case card.UsedAt != nil:
		writeCardError(w, r, chatops.ErrCardUsed)
		return card, nil, nil, false
	case card.Expired(time.Now()):
		writeCardError(w, r, chatops.ErrCardExpired)
		return card, nil, nil, false
	}
	inst := d.mgr.GetInstance(card.Database)
	if inst == nil || inst.Pool == nil || inst.Executor == nil {
		sreErrorCode(w, "the card's database is not monitored", "not_found",
			http.StatusNotFound)
		return card, nil, nil, false
	}
	current, err := store.NewActionStore(inst.Pool).GetByID(r.Context(), card.QueueID)
	if err != nil {
		sreErrorCode(w, "the card's action no longer exists", "not_found",
			http.StatusNotFound)
		return card, nil, nil, false
	}
	if approvalcard.ContentHash(*current) != card.CardHash {
		slog.Warn("approval card refused: content changed after it was sent",
			"queue_id", card.QueueID, "card", card.ID)
		reply(ch, a, "Refused: this action changed after the card was sent. "+
			"Review the current version in pg_sage.")
		sreErrorCode(w, "the action changed after this card was sent", "content_changed",
			http.StatusConflict)
		return card, nil, nil, false
	}
	return card, inst, current, true
}

func writeCardError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, chatops.ErrCardUnknown):
		sreErrorCode(w, "unknown approval card", "unknown_card", http.StatusNotFound)
	case errors.Is(err, chatops.ErrCardUsed):
		sreErrorCode(w, "this card was already used", "card_used", http.StatusConflict)
	case errors.Is(err, chatops.ErrCardExpired):
		sreErrorCode(w, "this card expired; open pg_sage to decide", "card_expired",
			http.StatusGone)
	default:
		internalError(w, r, "approval card", err)
	}
}

// approveCardFromChat approves exactly the SQL the card showed.
func approveCardFromChat(r *http.Request, as *store.ActionStore,
	inst *fleet.DatabaseInstance, card chatops.CardDelivery, current *store.QueuedAction,
	user auth.User) cardOutcome {
	body, refused := approveAndRunExpecting(r.Context(), as, inst.Executor, card.QueueID,
		user.ID, current.ProposedSQL)
	if refused != nil {
		msg := refused.msg
		if refused.approveErr != nil {
			msg = "the action is no longer awaiting approval"
		}
		return cardOutcome{body: map[string]any{"ok": false, "error": msg,
			"decision": "approve"}, text: "Not approved: " + msg + ".",
			status: http.StatusConflict}
	}
	body["decision"] = "approve"
	if body["executed"] != true {
		return cardOutcome{body: body, status: http.StatusOK, text: fmt.Sprintf(
			"Approved by %s, but the action did not run: %v", user.Email, body["error"])}
	}
	return cardOutcome{body: body, status: http.StatusOK, text: fmt.Sprintf(
		"Approved by %s: %q ran on %s (verification: %v). The verified result will "+
			"follow here.", user.Email, card.Title, inst.Name, body["verification_status"])}
}

// rejectCardFromChat rejects the item; the rejection keeps this exact
// change behind approval (operator-rejection rule).
func rejectCardFromChat(r *http.Request, as *store.ActionStore, card chatops.CardDelivery,
	a chatops.Action, user auth.User) cardOutcome {
	reason := fmt.Sprintf("rejected in %s by %s", a.Provider, user.Email)
	if err := as.Reject(r.Context(), card.QueueID, user.ID, reason); err != nil {
		slog.Warn("chat card reject failed", "queue_id", card.QueueID, "error", err)
		return cardOutcome{body: map[string]any{"ok": false, "decision": "deny",
			"error": "not awaiting approval"}, status: http.StatusConflict,
			text: "Not rejected: the action is no longer awaiting approval."}
	}
	return cardOutcome{body: map[string]any{"ok": true, "decision": "deny",
		"status": "rejected"}, status: http.StatusOK, text: fmt.Sprintf(
		"Rejected by %s. pg_sage will not run this change without a new approval.",
		user.Email)}
}

// snoozeCardFromChat defers the item; pg_sage sends the card again when
// the snooze ends.
func snoozeCardFromChat(r *http.Request, as *store.ActionStore, card chatops.CardDelivery,
	a chatops.Action, user auth.User) cardOutcome {
	reason := fmt.Sprintf("snoozed in %s by %s", a.Provider, user.Email)
	until, err := snoozeItem(r.Context(), as, card.QueueID, user.ID, chatSnooze, reason)
	if err != nil {
		slog.Warn("chat card snooze failed", "queue_id", card.QueueID, "error", err)
		return cardOutcome{body: map[string]any{"ok": false, "decision": "snooze",
			"error": "not awaiting approval"}, status: http.StatusConflict,
			text: "Not snoozed: the action is no longer awaiting approval."}
	}
	return cardOutcome{body: map[string]any{"ok": true, "decision": "snooze",
		"status": "snoozed", "snoozed_until": until}, status: http.StatusOK,
		text: fmt.Sprintf("Snoozed by %s until %s; pg_sage will ask again then.",
			user.Email, until.Format("2006-01-02 15:04 UTC"))}
}
