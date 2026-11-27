package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/store"
)

// One chat approval protocol (policy-08). Sage SRE proposals are decided
// through their approval cards like every queued action. The retired
// proposal-id buttons ("sage_approve" / "sage:a:<proposal id>") are still
// parsed for one release, only to answer them: they change no state.

// outdatedButtonReply answers a press of a retired proposal button.
const outdatedButtonReply = "This button is outdated; open pg_sage to decide."

// answerOutdated replies to a retired button, after the callback passed
// the same signature, scope, replay and role checks as any decision.
func answerOutdated(w http.ResponseWriter, ch notify.Channel, a chatops.Action) {
	slog.Info("chatops: outdated proposal button pressed", "channel", ch.ID,
		"provider", a.Provider)
	reply(ch, a, outdatedButtonReply)
	sreErrorCode(w, outdatedButtonReply, "outdated_button", http.StatusGone)
}

// syncSREProposal records a chat decision on the Sage SRE proposal an
// item backs at once (the action service's tick would otherwise).
func syncSREProposal(ctx context.Context, inst *fleet.DatabaseInstance,
	item *store.QueuedAction) {
	if inst == nil || inst.Actions == nil || item == nil {
		return
	}
	id, ok := sreaction.ProposalIDFromIdentityKey(item.IdentityKey)
	if !ok {
		return
	}
	p, err := inst.Actions.Get(ctx, id)
	if err == nil {
		err = inst.Actions.SyncProposal(ctx, p)
	}
	if err != nil {
		slog.Warn("chatops: proposal sync deferred to the next tick", "proposal", id,
			"queue_id", item.ID, "error", err)
	}
}
