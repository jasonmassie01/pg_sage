package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// endCurrentSession deletes the session the request's cookie names and
// audits the logout for the signed-in user. Logout still clears the cookie
// when this fails, so a failure is logged, not returned.
func endCurrentSession(r *http.Request, pool *pgxpool.Pool) {
	cookie, err := r.Cookie("sage_session")
	if err != nil || pool == nil {
		return
	}
	if err := auth.DeleteSession(r.Context(), pool, cookie.Value); err != nil {
		slog.Error("logout: deleting the session failed", "error", err)
		return
	}
	user := UserFromContext(r.Context())
	if user == nil {
		return
	}
	recordAuthEvent(r, pool, auth.AuthAuditEvent{
		Event: auth.AuditLogout, ActorUserID: user.ID, TargetUserID: user.ID,
	})
}

// signOutOtherSessionsHandler serves DELETE /api/v1/auth/sessions: it ends
// every session of the signed-in user except the one making the request.
func signOutOtherSessionsHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		cookie, err := r.Cookie("sage_session")
		if user == nil || err != nil || pool == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		ended, err := auth.EndUserSessions(r.Context(), pool, user.ID,
			auth.HashSessionToken(cookie.Value))
		if err != nil {
			internalError(w, r, "sign out other sessions", err)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditSessionsRevoked, ActorUserID: user.ID,
			TargetUserID: user.ID,
			Detail:       map[string]any{"count": ended, "scope": "others"},
		})
		jsonResponse(w, map[string]int{"revoked": ended})
	}
}
