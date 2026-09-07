package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
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

// loginRateLimiter tracks failed login attempts per email.
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	stop     chan struct{}
	stopOnce sync.Once
}

var loginLimiter = newLoginRateLimiter()

const (
	loginMaxAttempts = 5
	loginWindow      = 15 * time.Minute
	loginMaxEntries  = 10000
	loginCleanupFreq = 5 * time.Minute
)

func newLoginRateLimiter() *loginRateLimiter {
	l := &loginRateLimiter{
		attempts: make(map[string][]time.Time),
		stop:     make(chan struct{}),
	}
	go l.cleanupLoop()
	return l
}

// cleanupLoop periodically purges expired entries to bound
// memory growth from distributed login spray attacks. It exits
// when Stop is called, allowing tests and shutdown paths to
// reclaim the goroutine instead of leaking one per process.
func (l *loginRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(loginCleanupFreq)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.purgeExpired()
		}
	}
}

// Stop halts the cleanup goroutine. Idempotent — safe to call
// multiple times. After Stop, purgeExpired no longer runs in the
// background; allow/record/reset remain functional.
func (l *loginRateLimiter) Stop() {
	l.stopOnce.Do(func() {
		close(l.stop)
	})
}

// ShutdownLoginLimiter stops the package-level login rate limiter's
// background cleanup goroutine. Call during graceful shutdown so the
// goroutine doesn't outlive the process's API server.
func ShutdownLoginLimiter() {
	loginLimiter.Stop()
}

func (l *loginRateLimiter) purgeExpired() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-loginWindow)
	for email, attempts := range l.attempts {
		valid := attempts[:0]
		for _, t := range attempts {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(l.attempts, email)
		} else {
			l.attempts[email] = valid
		}
	}
}

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
	// If the key already exists, recording another attempt does not
	// grow the map. Always allow that case so an attacker cannot
	// freeze the limiter for real administrators by first filling the
	// map with random emails.
	if _, exists := l.attempts[email]; exists {
		l.attempts[email] = append(l.attempts[email], time.Now())
		return
	}
	// New key: if the map is at capacity, evict an expired entry
	// before inserting. If no expired entry exists, evict the oldest
	// tracked email. This bounds memory without silently dropping
	// tracking for the real target of a spray attack.
	if len(l.attempts) >= loginMaxEntries {
		l.evictOneLocked()
	}
	l.attempts[email] = append(l.attempts[email], time.Now())
}

// evictOneLocked removes one entry from the map. Caller must hold mu.
// Prefers entries whose most recent attempt is outside the window;
// otherwise evicts the entry with the oldest most-recent attempt.
func (l *loginRateLimiter) evictOneLocked() {
	cutoff := time.Now().Add(-loginWindow)
	var oldestEmail string
	var oldestLast time.Time
	for email, attempts := range l.attempts {
		if len(attempts) == 0 {
			delete(l.attempts, email)
			return
		}
		last := attempts[len(attempts)-1]
		if last.Before(cutoff) {
			delete(l.attempts, email)
			return
		}
		if oldestEmail == "" || last.Before(oldestLast) {
			oldestEmail = email
			oldestLast = last
		}
	}
	if oldestEmail != "" {
		delete(l.attempts, oldestEmail)
	}
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

		if !loginRateLimitDisabled &&
			!loginLimiter.allow(req.Email) {
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
			Secure:   isSecureRequest(r),
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

// oauthStateCookieName is the browser-bound CSRF cookie for the
// OAuth state token. It must match the state query param on callback.
const oauthStateCookieName = "oauth_state"

func oauthAuthorizeHandler(
	provider *auth.OAuthProvider,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			jsonError(w, "OAuth not configured",
				http.StatusNotFound)
			return
		}
		authURL, state, err := provider.AuthorizationURL()
		if err != nil {
			internalError(w, r, "oauth authorization url", err)
			return
		}
		// Bind state to this browser: only a request that echoes this
		// cookie on the callback can complete the flow. SameSite=Lax
		// still allows the top-level redirect back from the provider.
		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookieName,
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600, // 10 min — matches state TTL
		})
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

		// Cookie-bound state check: defeats login CSRF where an
		// attacker tricks a victim's browser into completing the
		// attacker's half-finished OAuth flow.
		var cookieState string
		if c, cerr := r.Cookie(oauthStateCookieName); cerr == nil {
			cookieState = c.Value
		}
		// Always clear the cookie before returning (success or fail).
		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookieName,
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})

		email, err := provider.Exchange(
			r.Context(), code, state, cookieState,
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

		if req.Role != auth.RoleAdmin {
			caller := UserFromContext(r.Context())
			if caller != nil && caller.ID == id {
				jsonError(w, "cannot change your own admin role",
					http.StatusForbidden)
				return
			}
		}

		if err := auth.UpdateUserRolePreservingAdmin(
			r.Context(), pool, id, req.Role,
		); err != nil {
			switch {
			case errors.Is(err, auth.ErrInvalidRole):
				jsonError(w, err.Error(),
					http.StatusBadRequest)
			case errors.Is(err, auth.ErrUserNotFound):
				jsonError(w, "user not found",
					http.StatusNotFound)
			case errors.Is(err, auth.ErrLastAdmin):
				jsonError(w, "cannot demote the last admin",
					http.StatusForbidden)
			default:
				internalError(w, r, "update user role", err)
			}
			return
		}
		jsonResponse(w, map[string]string{
			"status": "updated",
		})
	}
}
