package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

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
		setOAuthStateCookie(w, state)
		jsonResponse(w, map[string]string{"url": authURL})
	}
}

// setOAuthStateCookie binds state to this browser: only a request that
// echoes this cookie on the callback can complete the flow. SameSite=Lax
// still allows the top-level redirect back from the provider.
func setOAuthStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600, // 10 min — matches state TTL
	})
}

func clearOAuthStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
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
		code, state, cookieState, ok := oauthCallbackParams(w, r)
		if !ok {
			return
		}
		// Read before Exchange consumes the single-use state.
		link := provider.LinkIntentForState(state)
		identity, err := provider.Exchange(
			r.Context(), code, state, cookieState,
		)
		if err != nil {
			slog.Error("oauth exchange failed", "error", err)
			jsonError(w, "authentication failed",
				http.StatusUnauthorized)
			return
		}
		if link.UserID > 0 {
			completeOAuthLink(w, r, pool, providerName, identity, link)
			return
		}
		user, err := auth.FindOrCreateOAuthUser(
			r.Context(), pool, identity, providerName, defaultRole,
		)
		if err != nil {
			writeOAuthUserError(w, r, err)
			return
		}
		if startSession(w, r, pool, user.ID) {
			http.Redirect(w, r, "/", http.StatusFound)
		}
	}
}

// oauthCallbackParams reads code, state and the browser-bound state cookie,
// and clears the cookie. The cookie check defeats login CSRF where another
// party's half-finished OAuth flow completes in this browser.
func oauthCallbackParams(
	w http.ResponseWriter, r *http.Request,
) (code, state, cookieState string, ok bool) {
	code = r.URL.Query().Get("code")
	state = r.URL.Query().Get("state")
	if code == "" || state == "" {
		jsonError(w, "missing code or state parameter",
			http.StatusBadRequest)
		return "", "", "", false
	}
	if c, err := r.Cookie(oauthStateCookieName); err == nil {
		cookieState = c.Value
	}
	clearOAuthStateCookie(w)
	return code, state, cookieState, true
}

// startSession creates a session for userID and sets the session cookie.
// It reports false after writing an error response.
func startSession(
	w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, userID int,
) bool {
	sessionID, err := auth.CreateSession(r.Context(), pool, userID)
	if err != nil {
		jsonError(w, "failed to create session",
			http.StatusInternalServerError)
		return false
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
	return true
}

// writeOAuthUserError maps identity resolution failures without
// logging the caller's email (G6-B11).
func writeOAuthUserError(
	w http.ResponseWriter, r *http.Request, err error,
) {
	switch {
	case errors.Is(err, auth.ErrOAuthLinkRequired):
		slog.Warn("oauth login refused: account linking required")
		jsonError(w, "an account with this email already exists; "+
			"sign in with your password and link SSO from your profile, "+
			"or ask an administrator for an SSO link", http.StatusForbidden)
	case errors.Is(err, auth.ErrOAuthEmailUnverified):
		jsonError(w, "email not verified by identity provider",
			http.StatusUnauthorized)
	default:
		internalError(w, r, "resolve oauth user", err)
	}
}
