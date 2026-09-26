package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

// Principal model (G8-B05):
//   - Human sessions with role admin/operator are global operators of this
//     pg_sage install: they may act on any tenant, and every audit actor is
//     taken from the authenticated session, never from the request body.
//   - Agents authenticate with a tenant-bound agent token (minted by an admin
//     for an agent_identities row) on /api/v1/agent-api/ only. The tenant and
//     agent are taken from the token. Agents can list/read their own tenant's
//     deployments and create/read their own requests; they can never approve,
//     authorize, destroy, archive or mint tokens.
//   - A ping token is scoped to one deployment and may only record liveness.
const agentAPIPrefix = "/api/v1/agent-api/"

type agentPrincipalKey struct{}

// isAgentDBAgentAPIPath reports paths that authenticate with agent tokens
// instead of session cookies.
func isAgentDBAgentAPIPath(path string) bool {
	return strings.HasPrefix(path, agentAPIPrefix)
}

func agentPrincipalFromContext(ctx context.Context) (agentdb.AgentPrincipal, bool) {
	principal, ok := ctx.Value(agentPrincipalKey{}).(agentdb.AgentPrincipal)
	return principal, ok && principal.TenantID != "" && principal.AgentID != ""
}

func requireAgentPrincipal(st *agentdb.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			token := strings.TrimPrefix(header, "Bearer ")
			if token == "" || token == header {
				jsonError(w, "agent token required", http.StatusUnauthorized)
				return
			}
			principal, err := st.ValidateAgentToken(r.Context(), token)
			if err != nil {
				jsonError(w, "agent token invalid or expired", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), agentPrincipalKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func registerAgentDBAgentAPIRoutes(
	mux *http.ServeMux,
	st *agentdb.Store,
	authority *agentDBLiveAuthority,
) {
	agentOnly := requireAgentPrincipal(st)
	mux.Handle("GET "+agentAPIPrefix+"agent-dbs",
		agentOnly(http.HandlerFunc(agentAPIListHandler(st))))
	mux.Handle("GET "+agentAPIPrefix+"agent-dbs/{deployment_id}",
		agentOnly(http.HandlerFunc(agentAPIGetHandler(st))))
	mux.Handle("POST "+agentAPIPrefix+"agent-db-requests",
		agentOnly(http.HandlerFunc(agentAPICreateRequestHandler(st, authority))))
	mux.Handle("GET "+agentAPIPrefix+"agent-db-requests",
		agentOnly(http.HandlerFunc(agentAPIListRequestsHandler(st))))
}

func agentAPIListHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := agentPrincipalFromContext(r.Context())
		options, err := agentDBListOptions(r)
		if err != nil {
			agentDBError(w, err)
			return
		}
		options.TenantID = principal.TenantID
		page, err := st.ListPage(r.Context(), options)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, page)
	}
}

func agentAPIGetHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := agentPrincipalFromContext(r.Context())
		dep, err := st.GetForTenant(r.Context(), agentDBID(r), principal.TenantID)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, dep)
	}
}

func agentAPICreateRequestHandler(
	st *agentdb.Store,
	authority *agentDBLiveAuthority,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := agentPrincipalFromContext(r.Context())
		m, err := readJSONMap(r)
		if err != nil {
			agentDBError(w, err)
			return
		}
		m["tenant_id"], m["agent_id"] = principal.TenantID, principal.AgentID
		created, err := st.CreateRequest(r.Context(), requestCreateFromBody(r, m, authority))
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, created)
	}
}

func agentAPIListRequestsHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := agentPrincipalFromContext(r.Context())
		rows, err := st.RequestsForTenant(r.Context(), principal.TenantID)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"requests": rows})
	}
}

// agentDBMintAgentTokenHandler lets an admin mint a tenant-bound agent
// token; the tenant is the identity's, whatever the body says.
func agentDBMintAgentTokenHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := readJSONMap(r)
		if err != nil {
			agentDBError(w, err)
			return
		}
		expires := integer(m, "expires_seconds")
		if expires == 0 {
			expires = 7 * 24 * 3600
		}
		token, err := st.CreateAgentToken(r.Context(), r.PathValue("agent_id"),
			authenticatedActor(r), expires)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, token)
	}
}

// authenticatedActor names the session user for audit fields; request
// bodies can never choose who approved or created something (SURF-17).
func authenticatedActor(r *http.Request) string {
	user := UserFromContext(r.Context())
	if user == nil {
		return ""
	}
	if user.Email != "" {
		return user.Email
	}
	return liveRequesterID(r)
}
