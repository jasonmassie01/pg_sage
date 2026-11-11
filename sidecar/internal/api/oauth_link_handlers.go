package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// D7 account linking. A link binds an SSO identity to an existing account
// and is started either by that account's signed-in session or by an
// admin-issued one-time grant. The OAuth callback for a link never creates
// a user, and email alone never links an account.

const oauthLinkGrantPath = "/api/v1/auth/oauth/link-grant"

func registerAccountLinkRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool,
	provider *auth.OAuthProvider, providerName string,
) {
	route(mux, "POST "+oauthLinkGrantPath, gateSelfAuth,
		http.HandlerFunc(oauthLinkGrantHandler(provider, pool)))
	route(mux, "GET /api/v1/auth/sso", gateViewer,
		http.HandlerFunc(ssoStatusHandler(pool, provider, providerName)))
	route(mux, "DELETE /api/v1/users/{id}/oidc", gateAdmin,
		http.HandlerFunc(unlinkUserSSOHandler(pool)))
	route(mux, "POST /api/v1/users/{id}/oidc-link-grant", gateAdmin,
		http.HandlerFunc(issueLinkGrantHandler(pool)))
}

// oauthAuthorizeRouter serves /auth/oauth/authorize: intent=link starts a
// link for the signed-in user; anything else is a plain SSO login.
func oauthAuthorizeRouter(
	provider *auth.OAuthProvider, pool *pgxpool.Pool,
) http.HandlerFunc {
	login := oauthAuthorizeHandler(provider)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("intent") != "link" {
			login(w, r)
			return
		}
		if provider == nil {
			jsonError(w, "OAuth not configured", http.StatusNotFound)
			return
		}
		// The authorize route is public for login, so the link branch
		// resolves the session itself, before saying anything about SSO.
		user, err := sessionUser(r, pool)
		if errors.Is(err, auth.ErrSessionInvalid) {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err != nil {
			writeSessionStoreUnavailable(w, r, err)
			return
		}
		// The middleware gate does not see this public route (apiauth-03).
		if user.MustChangePassword {
			jsonError(w, passwordChangeRequiredCode, http.StatusForbidden)
			return
		}
		if ssoNotReady(w, provider) {
			return
		}
		startLinkRoundTrip(w, r, provider, user.ID, auth.LinkViaSession)
	}
}

// sessionUser resolves the request's session: the user, ErrSessionInvalid
// for no cookie or an invalid one, or ErrSessionStoreUnavailable when the
// store cannot answer (apiauth-02).
func sessionUser(r *http.Request, pool *pgxpool.Pool) (*auth.User, error) {
	cookie, err := r.Cookie("sage_session")
	if err != nil {
		return nil, auth.ErrSessionInvalid
	}
	return auth.ValidateSession(r.Context(), pool, cookie.Value)
}

func startLinkRoundTrip(
	w http.ResponseWriter, r *http.Request,
	provider *auth.OAuthProvider, userID int, via string,
) {
	authURL, state, err := provider.AuthorizationURLForLink(userID, via)
	if err != nil {
		internalError(w, r, "oauth link authorization url", err)
		return
	}
	setOAuthStateCookie(w, state)
	jsonResponse(w, map[string]string{"url": authURL})
}

// oauthLinkGrantHandler redeems an admin-issued grant (single use) and
// starts a link round trip for the user it was issued to.
func oauthLinkGrantHandler(
	provider *auth.OAuthProvider, pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if provider == nil {
			jsonError(w, "OAuth not configured", http.StatusNotFound)
			return
		}
		// Before redeeming: a single-use grant must not be spent on a
		// round trip that cannot start.
		if ssoNotReady(w, provider) {
			return
		}
		var req struct {
			Grant string `json:"grant"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		userID, err := auth.RedeemLinkGrant(r.Context(), pool, req.Grant)
		if errors.Is(err, auth.ErrLinkGrantInvalid) {
			jsonError(w, "invalid or expired link grant", http.StatusBadRequest)
			return
		}
		if err != nil {
			internalError(w, r, "redeem link grant", err)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditOIDCLinkGrantUsed, ActorUserID: userID, TargetUserID: userID,
		})
		startLinkRoundTrip(w, r, provider, userID, auth.LinkViaGrant)
	}
}

// completeOAuthLink finishes a link callback: it binds the identity to the
// intent's user, records the audit event, and signs that user in.
func completeOAuthLink(
	w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool,
	providerName string, identity auth.Identity, link auth.LinkIntent,
) {
	err := auth.LinkOAuthIdentity(r.Context(), pool, link.UserID, identity, providerName)
	if err != nil {
		writeOAuthUserError(w, r, err, link)
		return
	}
	recordAuthEvent(r, pool, auth.AuthAuditEvent{
		Event: auth.AuditOIDCLinked, ActorUserID: link.UserID, TargetUserID: link.UserID,
		Detail: map[string]any{"issuer": identity.Issuer, "via": link.Via},
	})
	if startSession(w, r, pool, link.UserID) {
		http.Redirect(w, r, "/#/profile?linked=1", http.StatusFound)
	}
}

func ssoStatusHandler(
	pool *pgxpool.Pool, provider *auth.OAuthProvider, providerName string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		st, err := auth.UserSSOStatus(r.Context(), pool, user.ID)
		if err != nil {
			internalError(w, r, "read sso status", err)
			return
		}
		jsonResponse(w, map[string]any{
			"linked": st.Linked, "issuer": st.Issuer,
			"password_login": st.PasswordLogin,
			"oauth_enabled":  provider != nil, "provider": providerName,
		})
	}
}

func unlinkUserSSOHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userPathID(w, r)
		if !ok {
			return
		}
		err := auth.UnlinkOAuthIdentity(r.Context(), pool, id)
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			jsonError(w, "user not found", http.StatusNotFound)
			return
		case errors.Is(err, auth.ErrOAuthNotLinked):
			jsonError(w, "user has no linked SSO identity", http.StatusConflict)
			return
		case err != nil:
			internalError(w, r, "unlink sso", err)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditOIDCUnlinked, ActorUserID: actorID(r), TargetUserID: id,
		})
		jsonResponse(w, map[string]string{"status": "unlinked"})
	}
}

func issueLinkGrantHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userPathID(w, r)
		if !ok {
			return
		}
		token, expires, err := auth.IssueLinkGrant(r.Context(), pool, id, actorID(r))
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			jsonError(w, "user not found", http.StatusNotFound)
			return
		case errors.Is(err, auth.ErrOAuthLinkConflict):
			jsonError(w, "user is already linked to an SSO identity", http.StatusConflict)
			return
		case err != nil:
			internalError(w, r, "issue link grant", err)
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditOIDCLinkGrantIssued, ActorUserID: actorID(r), TargetUserID: id,
			Detail: map[string]any{"expires_at": expires.UTC().Format(time.RFC3339)},
		})
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{"token": token, "expires_at": expires.UTC()})
	}
}

func userPathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		jsonError(w, "invalid user ID", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func actorID(r *http.Request) int {
	if user := UserFromContext(r.Context()); user != nil {
		return user.ID
	}
	return 0
}
