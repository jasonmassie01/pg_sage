package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/store"
)

// decide applies an authorized chat decision to the proposal's approval
// item through the same path as the browser.
func (d *chatopsDeps) decide(w http.ResponseWriter, r *http.Request, ch notify.Channel,
	a chatops.Action, user auth.User) {
	inst, p, err := d.findProposal(r.Context(), sre.UUID(a.ProposalID))
	switch {
	case errors.Is(err, sreaction.ErrProposalNotFound):
		sreErrorCode(w, "proposal not found", "not_found", http.StatusNotFound)
		return
	case err != nil:
		internalError(w, r, "chatops proposal", err)
		return
	case p.QueueID <= 0:
		sreErrorCode(w, "the proposal is not awaiting approval", "invalid_state",
			http.StatusConflict)
		return
	}
	as := store.NewActionStore(inst.Pool)
	var body map[string]any
	var text string
	if a.Decision == chatops.DecisionApprove {
		body, text, err = approveFromChat(r.Context(), as, inst, p, user)
	} else {
		body, text, err = denyFromChat(r.Context(), as, inst, p, a, user)
	}
	if err != nil {
		sreErrorCode(w, err.Error(), "invalid_state", http.StatusConflict)
		return
	}
	body["database"], body["proposal_id"] = inst.Name, string(p.ID)
	reply(ch, a, text)
	jsonResponse(w, body)
}

// findProposal finds the database whose action service holds a proposal.
func (d *chatopsDeps) findProposal(ctx context.Context,
	id sre.UUID) (*fleet.DatabaseInstance, sreaction.Proposal, error) {
	var failed error
	for _, inst := range d.mgr.Instances() {
		if inst == nil || inst.Actions == nil {
			continue
		}
		p, err := inst.Actions.Get(ctx, id)
		if err == nil {
			return inst, p, nil
		}
		if !errors.Is(err, sreaction.ErrProposalNotFound) &&
			!errors.Is(err, sre.ErrInvalidRequest) {
			failed = err
		}
	}
	if failed != nil {
		return nil, sreaction.Proposal{}, failed
	}
	return nil, sreaction.Proposal{}, sreaction.ErrProposalNotFound
}

func approveFromChat(ctx context.Context, as *store.ActionStore,
	inst *fleet.DatabaseInstance, p sreaction.Proposal,
	user auth.User) (map[string]any, string, error) {
	body, refused := approveAndRun(ctx, as, inst.Executor, p.QueueID, user.ID)
	if refused != nil {
		msg := refused.msg
		if refused.approveErr != nil {
			msg = "the proposal is no longer awaiting approval"
		}
		return nil, "", errors.New(msg)
	}
	body["decision"] = string(chatops.DecisionApprove)
	if body["executed"] != true {
		return body, fmt.Sprintf("Approved by %s, but the action did not run: %v",
			user.Email, body["error"]), nil
	}
	return body, fmt.Sprintf("Approved by %s: the cancel was executed on %s; "+
		"recovery is being verified.", user.Email, inst.Name), nil
}

func denyFromChat(ctx context.Context, as *store.ActionStore,
	inst *fleet.DatabaseInstance, p sreaction.Proposal, a chatops.Action,
	user auth.User) (map[string]any, string, error) {
	reason := fmt.Sprintf("denied in %s by %s", a.Provider, user.Email)
	if err := as.Reject(ctx, p.QueueID, user.ID, reason); err != nil {
		slog.Warn("chatops deny failed", "queue_id", p.QueueID, "error", err)
		return nil, "", errors.New("the proposal is no longer awaiting approval")
	}
	if err := inst.Actions.SyncProposal(ctx, p); err != nil {
		slog.Warn("chatops deny: proposal sync deferred to the next tick",
			"proposal", p.ID, "error", err)
	}
	return map[string]any{"ok": true, "decision": string(chatops.DecisionDeny),
			"queue_id": p.QueueID, "status": "rejected"}, fmt.Sprintf("Denied by %s.",
			user.Email), nil
}
