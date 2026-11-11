package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// Two-person principal changes (agentgov-06, §6.11). Outside
// single-operator mode a widening PATCH /api/v1/agents/{id} answers 202
// with the pending change; another admin, neither the requester nor the
// sponsor, confirms it with the same fields at
// POST /api/v1/agents/{id}/changes/{cid}/confirm within the TTL.

// requestChange records the widening patch as the agent's pending change.
func (a *agentRoutes) requestChange(w http.ResponseWriter, r *http.Request,
	cur agentguard.Principal, patch agentguard.Patch, body patchAgentBody) {
	if body.Status != nil {
		jsonError(w, "retire an agent on its own, not with a widening change",
			http.StatusUnprocessableEntity)
		return
	}
	pc, err := a.store.RequestChange(r.Context(), agentguard.ChangeRequest{
		PrincipalID: cur.ID, Patch: patch, Actor: authenticatedActor(r), TTL: a.changeTTL,
		Audit: governanceAudit(r, body.Reason)})
	if err != nil {
		agentError(w, r, "request agent change", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending", "change_id": pc.ID,
		"expires_at": pc.ExpiresAt, "requested_by": pc.RequestedBy, "patch": pc.Patch,
		"superseded_change_id": pc.SupersededID,
		"message": "this change widens what the agent may do; another admin, not its " +
			"sponsor, confirms it with the same fields"})
}

// changes lists the agent's pending change.
func (a *agentRoutes) changes(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.PendingChanges(r.Context(), r.PathValue("id"))
	if err != nil {
		agentError(w, r, "list agent changes", err)
		return
	}
	jsonResponse(w, map[string]any{"items": items})
}

type confirmChangeBody struct {
	SponsorUserID *int    `json:"sponsor_user_id"`
	Profile       *string `json:"profile"`
	EnvCeiling    *string `json:"env_ceiling"`
	Reason        string  `json:"reason"`
}

// confirmChange applies a pending change as the second admin.
func (a *agentRoutes) confirmChange(w http.ResponseWriter, r *http.Request) {
	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || cid <= 0 {
		writeChangeError(w, "change_not_found", "no such change", http.StatusNotFound)
		return
	}
	var body confirmChangeBody
	if !decodeStrict(w, r, &body) {
		return
	}
	patch := patchAgentBody{SponsorUserID: body.SponsorUserID, Profile: body.Profile,
		EnvCeiling: body.EnvCeiling}.patch()
	p, err := a.store.ConfirmChange(r.Context(), agentguard.ChangeConfirm{
		PrincipalID: r.PathValue("id"), ChangeID: cid, Patch: patch,
		Actor: authenticatedActor(r), Audit: governanceAudit(r, body.Reason)})
	if err != nil {
		agentError(w, r, "confirm agent change", err)
		return
	}
	jsonResponse(w, p)
}

func writeChangeError(w http.ResponseWriter, code, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

// changeError answers a two-person change error and reports whether err
// was one.
func changeError(w http.ResponseWriter, err error, msg string) bool {
	for _, c := range []struct {
		err    error
		code   string
		status int
	}{
		{agentguard.ErrChangeNotFound, "change_not_found", http.StatusNotFound},
		{agentguard.ErrChangeNotPending, "change_not_pending", http.StatusConflict},
		{agentguard.ErrChangeExpired, "change_expired", http.StatusConflict},
		{agentguard.ErrChangeMismatch, "change_mismatch", http.StatusConflict},
		{agentguard.ErrChangeWidens, "agent_changed", http.StatusConflict},
		{agentguard.ErrSecondAdminRequired, "second_admin_required", http.StatusForbidden},
		{agentguard.ErrSponsorCannotApprove, "sponsor_cannot_approve", http.StatusForbidden},
	} {
		if errors.Is(err, c.err) {
			writeChangeError(w, c.code, msg, c.status)
			return true
		}
	}
	return false
}
