package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-03: a signed-in user changes their own password, and an admin
// resets someone else's to a temporary password shown once. Neither route
// exists in Databricks Apps mode, where the platform owns sign-in.

type changePasswordRequest struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

// changePasswordHandler serves PUT /api/v1/auth/password. A wrong current
// password counts against the per-email login limiter, so a stolen session
// cannot guess the password faster than the login form allows.
func changePasswordHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		req, ok := decodeChangePassword(w, r, user)
		if !ok {
			return
		}
		if !reservePasswordCheck(user.Email) {
			jsonError(w, "too many password attempts, try again later",
				http.StatusTooManyRequests)
			return
		}
		keep := ""
		if cookie, err := r.Cookie("sage_session"); err == nil {
			keep = auth.HashSessionToken(cookie.Value)
		}
		ended, err := auth.ChangePassword(r.Context(), pool, user.ID,
			req.Current, req.New, keep)
		if err != nil {
			writePasswordChangeError(w, r, user.Email, err)
			return
		}
		passwordCheckSucceeded(user.Email)
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditPasswordChanged, ActorUserID: user.ID, TargetUserID: user.ID,
			Detail: map[string]any{"sessions_ended": ended},
		})
		jsonResponse(w, map[string]any{"status": "changed", "sessions_ended": ended})
	}
}

// decodeChangePassword reads and checks the body before the limiter is
// touched; it reports false after a 400 or 409.
func decodeChangePassword(
	w http.ResponseWriter, r *http.Request, user *auth.User,
) (changePasswordRequest, bool) {
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return req, false
	}
	if !user.HasPassword {
		jsonError(w, "no password to change: this account signs in with SSO",
			http.StatusConflict)
		return req, false
	}
	if req.Current == "" || req.New == "" {
		jsonError(w, "current_password and new_password required", http.StatusBadRequest)
		return req, false
	}
	if err := auth.ValidatePassword(user.Email, req.New); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func writePasswordChangeError(w http.ResponseWriter, r *http.Request, email string,
	err error) {
	switch {
	case errors.Is(err, auth.ErrCurrentPasswordWrong):
		// The reserved attempt stays counted.
		jsonError(w, "current password is incorrect", http.StatusUnauthorized)
		return
	case errors.Is(err, auth.ErrPasswordPolicy):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, auth.ErrNoPassword):
		jsonError(w, "no password to change: this account signs in with SSO",
			http.StatusConflict)
	case errors.Is(err, auth.ErrUserNotFound):
		jsonError(w, "authentication required", http.StatusUnauthorized)
	default:
		internalError(w, r, "change password", err)
	}
	// No password was checked: give the attempt back.
	refundPasswordCheck(email)
}

// reservePasswordCheck spends one of the email's login attempts.
func reservePasswordCheck(email string) bool {
	return loginRateLimitDisabled || loginLimiter.reserve(email)
}

func passwordCheckSucceeded(email string) {
	if !loginRateLimitDisabled {
		loginLimiter.reset(email)
	}
}

func refundPasswordCheck(email string) {
	if !loginRateLimitDisabled {
		loginLimiter.refund(email)
	}
}

// resetPasswordHandler serves POST /api/v1/users/{id}/password-reset
// (admin). It ends the user's sessions and returns a temporary password
// once; the user must replace it at the next sign-in. Admins change their
// own password with PUT /api/v1/auth/password instead.
func resetPasswordHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userIDParam(w, r)
		if !ok {
			return
		}
		if caller := UserFromContext(r.Context()); caller != nil && caller.ID == id {
			jsonError(w, "cannot reset your own password; change it from your account page",
				http.StatusForbidden)
			return
		}
		temp, ended, err := auth.ResetPassword(r.Context(), pool, id)
		if err != nil {
			writePasswordResetError(w, r, err)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditPasswordReset, ActorUserID: actorID(r), TargetUserID: id,
			Detail: map[string]any{"sessions_ended": ended},
		})
		w.Header().Set("Cache-Control", "no-store")
		jsonResponse(w, map[string]any{"temporary_password": temp,
			"must_change_password": true, "sessions_ended": ended})
	}
}

func writePasswordResetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		jsonError(w, "user not found", http.StatusNotFound)
	case errors.Is(err, auth.ErrNoPassword):
		jsonError(w, "no password to reset: this account signs in with SSO",
			http.StatusConflict)
	default:
		internalError(w, r, "reset password", err)
	}
}

// passwordChangeRequiredCode is the error a must-change session gets on
// every route except the ones allowedDuringPasswordChange lists.
const passwordChangeRequiredCode = "password_change_required"

// requirePasswordChanged holds a session whose password must change (an
// admin reset or a generated bootstrap password) to the change form.
func requirePasswordChanged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user != nil && user.MustChangePassword && !allowedDuringPasswordChange(r) {
			jsonError(w, passwordChangeRequiredCode, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowedDuringPasswordChange: the routes a must-change session can use.
// The path is matched exactly, so no other spelling reaches another route.
func allowedDuringPasswordChange(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/v1/auth/me", "/api/v1/auth/password", "/api/v1/auth/logout":
		return true
	}
	return false
}
