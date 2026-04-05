package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// loginRateLimiter tracks failed login attempts per email.
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

var loginLimiter = &loginRateLimiter{
	attempts: make(map[string][]time.Time),
}

const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
)

// allow returns true if the email is not rate-limited.
// It prunes expired entries on each call.
func (l *loginRateLimiter) allow(email string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-loginWindow)
	attempts := l.attempts[email]
	valid := attempts[:0]
	for _, t := range attempts {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	l.attempts[email] = valid
	return len(valid) < loginMaxAttempts
}

// record adds a failed attempt for the given email.
func (l *loginRateLimiter) record(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[email] = append(
		l.attempts[email], time.Now())
}

// reset clears failed attempts for the email on success.
func (l *loginRateLimiter) reset(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, email)
}

func loginHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if req.Email == "" || req.Password == "" {
			jsonError(w, "email and password required",
				http.StatusBadRequest)
			return
		}

		if !loginLimiter.allow(req.Email) {
			jsonError(w, "too many login attempts, "+
				"try again later",
				http.StatusTooManyRequests)
			return
		}

		user, err := auth.Authenticate(
			r.Context(), pool, req.Email, req.Password,
		)
		if err != nil {
			loginLimiter.record(req.Email)
			jsonError(w, "invalid credentials",
				http.StatusUnauthorized)
			return
		}

		loginLimiter.reset(req.Email)

		sessionID, err := auth.CreateSession(
			r.Context(), pool, user.ID,
		)
		if err != nil {
			jsonError(w, "failed to create session",
				http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "sage_session",
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(auth.SessionDuration.Seconds()),
		})

		jsonResponse(w, map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		})
	}
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
			Secure:   true,
			MaxAge:   -1,
		})
		jsonResponse(w, map[string]string{
			"status": "logged out",
		})
	}
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
			ID        int        `json:"id"`
			Email     string     `json:"email"`
			Role      string     `json:"role"`
			CreatedAt time.Time  `json:"created_at"`
			LastLogin *time.Time `json:"last_login"`
		}
		resp := make([]userResp, len(users))
		for i, u := range users {
			resp[i] = userResp{
				ID:        u.ID,
				Email:     u.Email,
				Role:      u.Role,
				CreatedAt: u.CreatedAt,
				LastLogin: u.LastLogin,
			}
		}
		jsonResponse(w, map[string]any{"users": resp})
	}
}

func createUserHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if req.Email == "" || req.Password == "" {
			jsonError(w, "email and password required",
				http.StatusBadRequest)
			return
		}
		if len(req.Password) < 8 {
			jsonError(w,
				"password must be at least 8 characters",
				http.StatusBadRequest)
			return
		}
		if req.Role == "" {
			req.Role = auth.RoleViewer
		}
		if !auth.IsValidRole(req.Role) {
			jsonError(w, "invalid role", http.StatusBadRequest)
			return
		}

		id, err := auth.CreateUser(
			r.Context(), pool,
			req.Email, req.Password, req.Role,
		)
		if err != nil {
			slog.Error("failed to create user",
				"email", req.Email, "error", err)
			jsonError(w, "failed to create user",
				http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{
			"id":    id,
			"email": req.Email,
			"role":  req.Role,
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

		// Ensure at least one admin remains after deletion.
		target, err := auth.GetUserByID(
			r.Context(), pool, id)
		if err != nil {
			jsonError(w, "user not found",
				http.StatusNotFound)
			return
		}
		if target.Role == auth.RoleAdmin {
			count, err := auth.CountAdmins(
				r.Context(), pool)
			if err != nil {
				slog.Error("failed to count admins",
					"error", err)
				jsonError(w,
					"internal error",
					http.StatusInternalServerError)
				return
			}
			if count <= 1 {
				jsonError(w,
					"cannot delete the last admin",
					http.StatusForbidden)
				return
			}
		}

		if err := auth.DeleteUser(
			r.Context(), pool, id,
		); err != nil {
			jsonError(w, "user not found",
				http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]string{
			"status": "deleted",
		})
	}
}

func oauthConfigHandler(
	provider *auth.OAuthProvider,
	providerName string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enabled := provider != nil
		jsonResponse(w, map[string]any{
			"enabled":  enabled,
			"provider": providerName,
		})
	}
}

func oauthAuthorizeHandler(
	provider *auth.OAuthProvider,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			jsonError(w, "OAuth not configured",
				http.StatusNotFound)
			return
		}
		authURL, err := provider.AuthorizationURL()
		if err != nil {
			jsonError(w, "failed to generate auth URL: "+err.Error(),
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]string{"url": authURL})
	}
}

func oauthCallbackHandler(
	provider *auth.OAuthProvider,
	pool *pgxpool.Pool,
	defaultRole string,
	providerName string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			jsonError(w, "OAuth not configured",
				http.StatusNotFound)
			return
		}
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if code == "" || state == "" {
			jsonError(w, "missing code or state parameter",
				http.StatusBadRequest)
			return
		}

		email, err := provider.Exchange(
			r.Context(), code, state,
		)
		if err != nil {
			slog.Error("oauth exchange failed",
				"error", err)
			jsonError(w, "authentication failed",
				http.StatusUnauthorized)
			return
		}

		user, err := auth.FindOrCreateOAuthUser(
			r.Context(), pool, email, providerName,
			defaultRole,
		)
		if err != nil {
			slog.Error("failed to create oauth user",
				"email", email, "error", err)
			jsonError(w, "failed to create user",
				http.StatusInternalServerError)
			return
		}

		sessionID, err := auth.CreateSession(
			r.Context(), pool, user.ID,
		)
		if err != nil {
			jsonError(w, "failed to create session",
				http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "sage_session",
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(auth.SessionDuration.Seconds()),
		})

		http.Redirect(w, r, "/", http.StatusFound)
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
		if err := auth.UpdateUserRole(
			r.Context(), pool, id, req.Role,
		); err != nil {
			jsonError(w, err.Error(),
				http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]string{
			"status": "updated",
		})
	}
}
