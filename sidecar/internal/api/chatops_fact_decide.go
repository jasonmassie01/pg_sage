package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Fact cards from chat (roadmap 2.3), on the approval-card rules: the
// callback is signed, processed once and attributed to a mapped operator
// or admin (callbackHandler); the token must name a fact card sent to this
// channel, unused and unexpired, whose fact has not changed since. The
// token is consumed before the decision, so a card decides at most once.

func (d *chatopsDeps) decideFactCard(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, a chatops.Action, user auth.User) {
	card, store, ok := d.checkFactCard(w, r, ch, a)
	if !ok {
		return
	}
	if _, err := d.factCards.Consume(r.Context(), a.FactToken, user.ID, a.Decision,
		a.MessageID); err != nil {
		writeCardError(w, r, err)
		return
	}
	f, err := store.Decide(r.Context(), card.FactID, facts.Decision{
		Confirm: a.Decision == chatops.DecisionApprove, Actor: user.Email,
		Note: "decided in " + a.Provider, ExpectHash: card.FactHash})
	if err != nil {
		reply(ch, a, "Not recorded: the fact changed or was already decided. Review it "+
			"in pg_sage.")
		writeFactError(w, r, err)
		return
	}
	verb := "Rejected"
	if f.Status == facts.StatusConfirmed {
		verb = "Confirmed"
	}
	reply(ch, a, fmt.Sprintf("%s by %s: %s (fact #%d on %s).", verb, user.Email,
		f.Describe(), f.ID, card.Database))
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "fact_id": f.ID,
		"status": f.Status, "database": card.Database}); err != nil {
		slog.Warn("chat fact card response not written", "fact_id", f.ID, "error", err)
	}
}

// checkFactCard verifies the fact card a token names: issued to this
// channel, unused, unexpired, on a monitored database, unchanged.
func (d *chatopsDeps) checkFactCard(w http.ResponseWriter, r *http.Request,
	ch notify.Channel, a chatops.Action) (chatops.FactCardDelivery, *facts.Store, bool) {
	card, err := d.factCards.Lookup(r.Context(), a.FactToken)
	switch {
	case err != nil:
		writeCardError(w, r, err)
		return card, nil, false
	case card.ChannelID != ch.ID:
		slog.Warn("fact card used from another channel", "card", card.ID,
			"issued_to", card.ChannelID, "used_from", ch.ID)
		sreErrorCode(w, "this card was sent to another channel", "wrong_channel",
			http.StatusForbidden)
		return card, nil, false
	case card.UsedAt != nil:
		writeCardError(w, r, chatops.ErrCardUsed)
		return card, nil, false
	case card.Expired(time.Now()):
		writeCardError(w, r, chatops.ErrCardExpired)
		return card, nil, false
	}
	inst := d.mgr.GetInstance(card.Database)
	if inst == nil || inst.Pool == nil {
		sreErrorCode(w, "the card's database is not monitored", "not_found",
			http.StatusNotFound)
		return card, nil, false
	}
	store := facts.NewStore(inst.Pool)
	cur, err := store.Get(r.Context(), card.FactID)
	if errors.Is(err, facts.ErrNotFound) || (err == nil && cur.Hash() != card.FactHash) {
		reply(ch, a, "Refused: this fact changed after the card was sent. Review the "+
			"current version in pg_sage.")
		sreErrorCode(w, "the fact changed after this card was sent", "content_changed",
			http.StatusConflict)
		return card, nil, false
	}
	if err != nil {
		internalError(w, r, "fact card", err)
		return card, nil, false
	}
	return card, store, true
}
