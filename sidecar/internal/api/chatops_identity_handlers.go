package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/chatops"
)

// Chat identity mappings: an admin links a Slack or Telegram user to a
// pg_sage account. Only mapped users can decide in chat, with the
// account's current role.

func registerChatOpsIdentityRoutes(mux *http.ServeMux, ids *chatops.Store) {
	adminOnly := RequireRole("admin")
	mux.Handle("GET /api/v1/chatops/identities", adminOnly(listIdentitiesHandler(ids)))
	mux.Handle("POST /api/v1/chatops/identities", adminOnly(linkIdentityHandler(ids)))
	mux.Handle("DELETE /api/v1/chatops/identities/{id}",
		adminOnly(unlinkIdentityHandler(ids)))
}

func listIdentitiesHandler(ids *chatops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := ids.List(r.Context())
		if err != nil {
			internalError(w, r, "chatops identities", err)
			return
		}
		if items == nil {
			items = []chatops.Identity{}
		}
		jsonResponse(w, map[string]any{"items": items})
	}
}

func linkIdentityHandler(ids *chatops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req chatops.Identity
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			sreErrorCode(w, "invalid JSON", "invalid_request", http.StatusBadRequest)
			return
		}
		req.ID, req.UserEmail = 0, ""
		req.CreatedBy = UserFromContext(r.Context()).ID
		got, err := ids.Link(r.Context(), req)
		switch {
		case errors.Is(err, chatops.ErrInvalidIdentity):
			sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
		case errors.Is(err, chatops.ErrIdentityConflict):
			sreErrorCode(w, err.Error(), "conflict", http.StatusConflict)
		case err != nil:
			internalError(w, r, "chatops link", err)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(got)
		}
	}
}

func unlinkIdentityHandler(ids *chatops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id <= 0 {
			sreErrorCode(w, "invalid identity id", "invalid_request", http.StatusBadRequest)
			return
		}
		err = ids.Unlink(r.Context(), id)
		switch {
		case errors.Is(err, chatops.ErrNotFound):
			sreErrorCode(w, "identity not found", "not_found", http.StatusNotFound)
		case err != nil:
			internalError(w, r, "chatops unlink", err)
		default:
			jsonResponse(w, map[string]any{"ok": true, "id": id})
		}
	}
}
