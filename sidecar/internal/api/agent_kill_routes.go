package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
)

// The agent kill switch, freeze and unfreeze (spec §6.10, §8.3):
//   - POST /api/v1/agents/kill (admin) {scope, id?, reason} → 200 with the
//     per-database report;
//   - POST /api/v1/agents/kill/release (admin) {scope, database?, reason}
//     lifts a fleet or database flag → 202 (two admins);
//   - POST /api/v1/agents/{id}/freeze (operator) {reason} → 200;
//   - POST /api/v1/agents/{id}/unfreeze (admin) {reason} → 202, pending
//     until a second admin repeats it after a kill.
//
// Kill and freeze are narrowing: they work under the emergency stop and at
// every trust level. The actor is always the session user. Without a
// control database or databases the routes answer 503.

// AgentKillSwitch is the kill switch (*agentguard.Switch).
type AgentKillSwitch interface {
	Kill(context.Context, agentguard.KillRequest) (agentguard.KillReport, error)
	Freeze(context.Context, agentguard.FreezeRequest) (agentguard.KillReport, error)
	Unfreeze(context.Context, agentguard.UnfreezeRequest) (agentguard.UnfreezeResult, error)
	Release(context.Context, agentguard.ReleaseRequest) (agentguard.UnfreezeResult, error)
}

const maxAgentKillBody = 4 << 10

func registerAgentKillRoutes(mux *http.ServeMux, sw AgentKillSwitch) {
	operatorUp := RequireRole("admin", "operator")
	admin := RequireRole("admin")
	h := agentKillHandlers{sw: sw}
	mux.Handle("POST /api/v1/agents/kill", admin(http.HandlerFunc(h.kill)))
	mux.Handle("POST /api/v1/agents/kill/release", admin(http.HandlerFunc(h.release)))
	mux.Handle("POST /api/v1/agents/{id}/freeze", operatorUp(http.HandlerFunc(h.freeze)))
	mux.Handle("POST /api/v1/agents/{id}/unfreeze", admin(http.HandlerFunc(h.unfreeze)))
}

type agentKillHandlers struct{ sw AgentKillSwitch }

// decode reads a strict JSON body into v; false means the answer is sent.
func (h agentKillHandlers) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if h.sw == nil {
		sreErrorCode(w, "the agent kill switch needs mode: meta or agents.control_database "+
			"and at least one monitored database", "unavailable",
			http.StatusServiceUnavailable)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentKillBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		sreErrorCode(w, "malformed body: "+err.Error(), "invalid_arguments",
			http.StatusUnprocessableEntity)
		return false
	}
	return true
}

func (h agentKillHandlers) kill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope  string `json:"scope"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	rep, err := h.sw.Kill(r.Context(), agentguard.KillRequest{
		Scope: agentguard.KillScope(body.Scope), ID: body.ID, Reason: body.Reason,
		Actor: authenticatedActor(r)})
	if err != nil {
		writeAgentKillError(w, r, err)
		return
	}
	jsonResponse(w, rep)
}

func (h agentKillHandlers) freeze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	rep, err := h.sw.Freeze(r.Context(), agentguard.FreezeRequest{
		PrincipalID: r.PathValue("id"), Reason: body.Reason, Actor: authenticatedActor(r)})
	if err != nil {
		writeAgentKillError(w, r, err)
		return
	}
	jsonResponse(w, rep)
}

func (h agentKillHandlers) unfreeze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	res, err := h.sw.Unfreeze(r.Context(), agentguard.UnfreezeRequest{
		PrincipalID: r.PathValue("id"), Reason: body.Reason, Actor: authenticatedActor(r),
		ActorUserID: sessionUserID(r)})
	writeUnfreeze(w, r, res, err)
}

func (h agentKillHandlers) release(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope    string `json:"scope"`
		Database string `json:"database"`
		Reason   string `json:"reason"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	res, err := h.sw.Release(r.Context(), agentguard.ReleaseRequest{
		Scope: agentguard.KillScope(body.Scope), Database: body.Database,
		Reason: body.Reason, Actor: authenticatedActor(r), ActorUserID: sessionUserID(r)})
	writeUnfreeze(w, r, res, err)
}

func sessionUserID(r *http.Request) int {
	if u := UserFromContext(r.Context()); u != nil {
		return u.ID
	}
	return 0
}

// writeUnfreeze answers 202: applied, or pending a second admin.
func writeUnfreeze(w http.ResponseWriter, r *http.Request, res agentguard.UnfreezeResult,
	err error) {
	if err != nil {
		writeAgentKillError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(res)
}

func writeAgentKillError(w http.ResponseWriter, r *http.Request, err error) {
	var withheld *executor.WithheldError
	switch {
	case errors.As(err, &withheld):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"verdict": "blocked",
			"reason_code": withheld.Decision.BlockedReason, "error": err.Error()})
	case errors.Is(err, agentguard.ErrInvalid):
		sreErrorCode(w, err.Error(), "invalid_arguments", http.StatusUnprocessableEntity)
	case errors.Is(err, agentguard.ErrNotFound):
		sreErrorCode(w, err.Error(), "not_found", http.StatusNotFound)
	case errors.Is(err, agentguard.ErrUnavailable),
		errors.Is(err, agentguard.ErrEncryptionKeyRequired):
		sreErrorCode(w, err.Error(), "unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, agentguard.ErrNotFrozen):
		sreErrorCode(w, err.Error(), "not_frozen", http.StatusConflict)
	case errors.Is(err, agentguard.ErrRetired):
		sreErrorCode(w, err.Error(), "retired", http.StatusConflict)
	case errors.Is(err, agentguard.ErrSponsorCannotApprove):
		sreErrorCode(w, err.Error(), "sponsor_cannot_approve", http.StatusForbidden)
	case errors.Is(err, agentguard.ErrApprovalRequired):
		sreErrorCode(w, err.Error(), "approval_required", http.StatusForbidden)
	default:
		internalError(w, r, "agent kill switch", err)
	}
}
