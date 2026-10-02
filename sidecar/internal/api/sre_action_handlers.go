package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Sage SRE action routes (AI-SRE-SPEC §9): operators propose an
// investigation's evidence-matched mitigation and request its execution,
// which queues exactly one item in the existing approval flow. Approval
// is the existing /api/v1/actions/{id}/approve (attribution, single
// use); nothing here executes.

func registerSREActionRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewerUp := RequireRole("admin", "operator", "viewer")
	operatorUp := RequireRole("admin", "operator")
	base := sreInvestigationsPath + "/{id}/proposals"
	mux.Handle("GET "+base, viewerUp(proposalListHandler(mgr)))
	mux.Handle("POST "+base, operatorUp(proposeHandler(mgr)))
	mux.Handle("POST "+base+"/{pid}/request", operatorUp(requestExecutionHandler(mgr)))
}

// actionService resolves {db} to its action service.
func actionService(w http.ResponseWriter, mgr *fleet.DatabaseManager,
	name string) (*sreaction.ActionService, bool) {
	if _, ok := investigationService(w, mgr, name); !ok {
		return nil, false
	}
	inst := mgr.GetInstance(name)
	if inst == nil || inst.Actions == nil {
		sreErrorCode(w, "actions unavailable for this database", "metadata_unavailable",
			http.StatusServiceUnavailable)
		return nil, false
	}
	return inst.Actions, true
}

// sreActionError writes an action error's canonical code.
func sreActionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sreaction.ErrProposalNotFound):
		sreErrorCode(w, "proposal not found", "not_found", http.StatusNotFound)
	case errors.Is(err, sreaction.ErrPolicyBlocked):
		sreErrorCode(w, err.Error(), "policy_blocked", http.StatusConflict)
	case errors.Is(err, sreaction.ErrProposalState):
		sreErrorCode(w, err.Error(), "invalid_state", http.StatusConflict)
	case errors.Is(err, sreaction.ErrHandoffBlocked):
		sreErrorCode(w, err.Error(), "metadata_unavailable",
			http.StatusServiceUnavailable)
	default:
		sreErrorResponse(w, r, err)
	}
}

func proposalListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := actionService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		views, err := a.Views(r.Context(), sre.UUID(r.PathValue("id")))
		if err != nil {
			sreActionError(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"database": a.Database(), "items": views})
	}
}

func proposeHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := actionService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		p, err := a.Propose(r.Context(), sre.UUID(r.PathValue("id")), proposalActor(r))
		if err != nil {
			sreActionError(w, r, err)
			return
		}
		writeProposal(w, r, a, p)
	}
}

func requestExecutionHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := actionService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		id := sre.UUID(r.PathValue("pid"))
		if _, err := sre.ParseUUID(string(id)); err != nil {
			sreActionError(w, r, err)
			return
		}
		p, err := a.Get(r.Context(), id)
		if err == nil && p.InvestigationID != sre.UUID(r.PathValue("id")) {
			err = sreaction.ErrProposalNotFound
		}
		if err == nil {
			p, err = a.RequestExecution(r.Context(), id, proposalActor(r))
		}
		if err != nil {
			sreActionError(w, r, err)
			return
		}
		writeProposal(w, r, a, p)
	}
}

func writeProposal(w http.ResponseWriter, r *http.Request, a *sreaction.ActionService,
	p sreaction.Proposal) {
	v, err := a.View(r.Context(), p)
	if err != nil {
		sreActionError(w, r, err)
		return
	}
	jsonResponse(w, v)
}

// proposalActor is the signed-in operator the timeline attributes to.
func proposalActor(r *http.Request) string {
	return fmt.Sprintf("user:%d", UserFromContext(r.Context()).ID)
}
