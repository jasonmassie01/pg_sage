package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// loginRateLimitDisabled reads PG_SAGE_DISABLE_LOGIN_RATE_LIMIT=1
// at process start. Purpose: let the Playwright suite exercise dozens
// of logins per email without tripping the 5-per-15-min limiter. Never
// set this in production — it disables brute-force protection.
var loginRateLimitDisabled = os.Getenv(
	"PG_SAGE_DISABLE_LOGIN_RATE_LIMIT",
) == "1"

func loginHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeLogin(w, r)
		if !ok {
			return
		}
		if !loginRateLimitDisabled &&
			!loginLimiter.reserve(req.Email) {
			auditPasswordFailure(r, pool, req.Email, "rate_limited")
			jsonError(w, "too many login attempts, "+
				"try again later",
				http.StatusTooManyRequests)
			return
		}

		user, err := auth.Authenticate(
			r.Context(), pool, req.Email, req.Password,
		)
		if err != nil {
			auditPasswordFailure(r, pool, req.Email, "invalid_credentials")
			jsonError(w, "invalid credentials",
				http.StatusUnauthorized)
			return
		}

		loginLimiter.reset(req.Email)
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditLoginSucceeded, ActorUserID: user.ID,
			TargetUserID: user.ID, Detail: map[string]any{"method": "password"},
		})

		if !setLoginSession(w, r, pool, user.ID) {
			return
		}
		jsonResponse(w, map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		})
	}
}

// setLoginSession creates a session and sets its cookie (Secure only when
// the request arrived over TLS); it reports false after a 500.
func setLoginSession(
	w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, userID int,
) bool {
	sessionID, err := auth.CreateSession(r.Context(), pool, userID)
	if err != nil {
		jsonError(w, "failed to create session", http.StatusInternalServerError)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "sage_session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionDuration.Seconds()),
	})
	return true
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// decodeLogin reads the login body; it reports false after a 400.
func decodeLogin(w http.ResponseWriter, r *http.Request) (loginRequest, bool) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return req, false
	}
	if req.Email == "" || req.Password == "" {
		jsonError(w, "email and password required", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// auditPasswordFailure records a refused password login against the
// targeted account when it exists (0 otherwise); the email is not stored.
func auditPasswordFailure(r *http.Request, pool *pgxpool.Pool, email, reason string) {
	if pool == nil {
		return
	}
	target, err := auth.UserIDByEmail(r.Context(), pool, email)
	if err != nil && !errors.Is(err, auth.ErrUserNotFound) {
		slog.Warn("login audit: looking up the targeted account failed", "error", err)
	}
	recordAuthEvent(r, pool, auth.AuthAuditEvent{
		Event: auth.AuditLoginFailed, TargetUserID: target,
		Detail: map[string]any{"method": "password", "reason": reason},
	})
}

func logoutHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sage_session")
		if err == nil {
			_ = auth.DeleteSession(
				r.Context(), pool, cookie.Value,
			)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "sage_session",
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   isSecureRequest(r),
			MaxAge:   -1,
		})
		jsonResponse(w, map[string]string{
			"status": "logged out",
		})
	}
}

func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

func meHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "not authenticated",
				http.StatusUnauthorized)
			return
		}
		jsonResponse(w, map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		})
	}
}

func listUsersHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := auth.ListUsers(r.Context(), pool)
		if err != nil {
			jsonError(w, "failed to list users",
				http.StatusInternalServerError)
			return
		}
		type userResp struct {
			ID            int        `json:"id"`
			Email         string     `json:"email"`
			Role          string     `json:"role"`
			CreatedAt     time.Time  `json:"created_at"`
			LastLogin     *time.Time `json:"last_login"`
			SSOLinked     bool       `json:"sso_linked"`
			SSOIssuer     string     `json:"sso_issuer"`
			PasswordLogin bool       `json:"password_login"`
		}
		resp := make([]userResp, len(users))
		for i, u := range users {
			resp[i] = userResp{
				ID: u.ID, Email: u.Email, Role: u.Role,
				CreatedAt: u.CreatedAt, LastLogin: u.LastLogin,
				SSOLinked: u.SSOIssuer != "", SSOIssuer: u.SSOIssuer,
				PasswordLogin: u.PasswordLogin,
			}
		}
		jsonResponse(w, map[string]any{"users": resp})
	}
}

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
	// SSOOnly creates a user with no password who signs in only after an
	// admin-issued link grant binds an SSO identity (D7).
	SSOOnly bool `json:"sso_only"`
}

// validate returns a client-facing error message, or "" when valid.
func (req *createUserRequest) validate() string {
	if req.Role == "" {
		req.Role = auth.RoleViewer
	}
	switch {
	case req.SSOOnly && req.Email == "":
		return "email required"
	case req.SSOOnly && req.Password != "":
		return "an SSO-only user cannot have a password"
	case !req.SSOOnly && (req.Email == "" || req.Password == ""):
		return "email and password required"
	case !req.SSOOnly && len(req.Password) < 8:
		return "password must be at least 8 characters"
	case !auth.IsValidRole(req.Role):
		return "invalid role"
	}
	return ""
}

func createUserHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if msg := req.validate(); msg != "" {
			jsonError(w, msg, http.StatusBadRequest)
			return
		}
		var id int
		var err error
		if req.SSOOnly {
			id, err = auth.CreateSSOOnlyUser(r.Context(), pool, req.Email, req.Role)
		} else {
			id, err = auth.CreateUser(r.Context(), pool,
				req.Email, req.Password, req.Role)
		}
		if err != nil {
			slog.Error("failed to create user",
				"email", req.Email, "error", err)
			jsonError(w, "failed to create user",
				http.StatusConflict)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditUserCreated, ActorUserID: actorID(r), TargetUserID: id,
			Detail: map[string]any{"role": req.Role, "sso_only": req.SSOOnly},
		})
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{
			"id":       id,
			"email":    req.Email,
			"role":     req.Role,
			"sso_only": req.SSOOnly,
		})
	}
}

func deleteUserHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil || id < 1 {
			jsonError(w, "invalid user ID",
				http.StatusBadRequest)
			return
		}

		// Prevent self-deletion.
		caller := UserFromContext(r.Context())
		if caller != nil && caller.ID == id {
			jsonError(w, "cannot delete your own account",
				http.StatusForbidden)
			return
		}

		if err := auth.DeleteUserPreservingAdmin(
			r.Context(), pool, id,
		); err != nil {
			switch {
			case errors.Is(err, auth.ErrLastAdmin):
				jsonError(w,
					"cannot delete the last admin",
					http.StatusForbidden)
			case errors.Is(err, auth.ErrUserNotFound):
				jsonError(w, "user not found",
					http.StatusNotFound)
			default:
				internalError(w, r, "delete user", err)
			}
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditUserDeleted, ActorUserID: actorID(r), TargetUserID: id,
		})
		jsonResponse(w, map[string]string{
			"status": "deleted",
		})
	}
}

func updateUserRoleHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil || id < 1 {
			jsonError(w, "invalid user ID",
				http.StatusBadRequest)
			return
		}
		var req struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}

		if req.Role != auth.RoleAdmin {
			caller := UserFromContext(r.Context())
			if caller != nil && caller.ID == id {
				jsonError(w, "cannot change your own admin role",
					http.StatusForbidden)
				return
			}
		}

		oldRole, _ := auth.UserRole(r.Context(), pool, id)
		if err := auth.UpdateUserRolePreservingAdmin(
			r.Context(), pool, id, req.Role,
		); err != nil {
			writeRoleUpdateError(w, r, err)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditUserRoleChanged, ActorUserID: actorID(r), TargetUserID: id,
			Detail: map[string]any{"old_role": oldRole, "new_role": req.Role,
				"source": "api"},
		})
		jsonResponse(w, map[string]string{
			"status": "updated",
		})
	}
}

func writeRoleUpdateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidRole):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, auth.ErrUserNotFound):
		jsonError(w, "user not found", http.StatusNotFound)
	case errors.Is(err, auth.ErrLastAdmin):
		jsonError(w, "cannot demote the last admin", http.StatusForbidden)
	default:
		internalError(w, r, "update user role", err)
	}
}
