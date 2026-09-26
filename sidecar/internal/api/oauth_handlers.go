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

		identity, err := provider.Exchange(
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
			r.Context(), pool, identity, providerName,
			defaultRole,
		)
		if err != nil {
			writeOAuthUserError(w, r, err)
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

// writeOAuthUserError maps identity resolution failures without
// logging the caller's email (G6-B11).
func writeOAuthUserError(
	w http.ResponseWriter, r *http.Request, err error,
) {
	switch {
	case errors.Is(err, auth.ErrOAuthLinkRequired):
		slog.Warn("oauth login refused: account linking required")
		jsonError(w, "an account with this email already exists; "+
			"ask an administrator to link it", http.StatusForbidden)
	case errors.Is(err, auth.ErrOAuthEmailUnverified):
		jsonError(w, "email not verified by identity provider",
			http.StatusUnauthorized)
	default:
		internalError(w, r, "resolve oauth user", err)
	}
}
