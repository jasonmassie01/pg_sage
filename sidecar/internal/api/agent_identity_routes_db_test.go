package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcpauth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-03 end to end on real PostgreSQL: an admin binds an OAuth
// identity through the API and the MCP endpoint then accepts that
// identity's access token for the bound principal; unbinding refuses it
// at once. The IdP is in-process (loopback http, as mcp.oauth allows).

type identityFixture struct {
	pool  *pgxpool.Pool
	admin *auth.User
	idp   *oauthIDP
	api   http.Handler // the real router as the admin
	mcp   http.Handler // the MCP routes with the real resolver and store
	seen  *capture
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	pool := surfacePool(t)
	fx := &identityFixture{pool: pool, admin: tokenRouteUser(t, pool, auth.RoleAdmin),
		idp: newOAuthIDP(t), seen: &capture{}}
	fx.api = fx.router(t, fx.admin)
	v := oauthValidator(t, fx.idp, mcpauth.NewDBResolver(pool))
	mux := http.NewServeMux()
	registerMCPRoutes(mux, fx.seen, nil, v, agentguard.NewStore(pool))
	fx.mcp = mux
	return fx
}

func (fx *identityFixture) router(t *testing.T, user *auth.User) http.Handler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.MCP.OAuth.Enabled = true
	cfg.MCP.OAuth.Resource = oauthResource
	cfg.MCP.OAuth.Issuers = []config.MCPOAuthIssuer{{Issuer: fx.idp.srv.URL}}
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			next.ServeHTTP(w, r)
		})
	}
	return newTestRouter(nil, cfg, fx.pool, nil, nil, nil, &RuntimeDeps{}, inject)
}

func (fx *identityFixture) bindBody(subject string) string {
	return bindBody(fx.idp.srv.URL, subject)
}

func (fx *identityFixture) unbindPath(id, subject string) string {
	return agentsPath + "/" + id + "/identities?issuer=" + url.QueryEscape(fx.idp.srv.URL) +
		"&subject=" + url.QueryEscape(subject)
}

// mcpCall sends an MCP request with an access token for subject.
func (fx *identityFixture) mcpCall(t *testing.T, subject string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	fx.mcp.ServeHTTP(w, oauthRequest("/api/v1/mcp", fx.idp.token(t, subject, oauthResource)))
	return w.Code, w.Body.String()
}

func TestIdentityRoutesDB_BindReachesMCPAndUnbindRefuses(t *testing.T) {
	fx := newIdentityFixture(t)
	id := createAgent(t, fx.api, fx.admin.ID, "stage")["id"].(string)
	subject := agentName("oauth-client")

	code, body := fx.mcpCall(t, subject)
	require.Equal(t, http.StatusForbidden, code, body)
	require.Contains(t, body, "identity_unbound")

	ev := oneEvent(t, fx.pool, fx.admin.ID, auth.AuditAgentIdentityBound, func() {
		resp := callOK(t, fx.api, http.MethodPost, agentsPath+"/"+id+"/identities",
			fx.bindBody(subject), http.StatusCreated)
		b := decodeObject(t, resp)
		require.Equal(t, fx.idp.srv.URL, b["issuer"])
		require.Equal(t, subject, b["subject"])
		require.Equal(t, id, b["principal_id"])
		require.Equal(t, fx.admin.Email, b["created_by"])
	})
	require.Equal(t, id, ev.Detail["principal_id"])
	require.Equal(t, fx.admin.ID, ev.Target, "the sponsor is the event's target")

	code, body = fx.mcpCall(t, subject)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 1, fx.seen.calls)
	require.Equal(t, id, fx.seen.identity.Principal.ID)
	require.Equal(t, "principal:"+id, fx.seen.principal.Actor)

	list := decodeObject(t, callOK(t, fx.api, http.MethodGet,
		agentsPath+"/"+id+"/identities", "", http.StatusOK))
	items := list["items"].([]any)
	require.Len(t, items, 1)
	require.Equal(t, subject, items[0].(map[string]any)["subject"])

	oneEvent(t, fx.pool, fx.admin.ID, auth.AuditAgentIdentityUnbound, func() {
		callOK(t, fx.api, http.MethodDelete, fx.unbindPath(id, subject), "", http.StatusOK)
	})
	code, body = fx.mcpCall(t, subject)
	require.Equal(t, http.StatusForbidden, code, body)
	require.Contains(t, body, "identity_unbound")
	require.Equal(t, 1, fx.seen.calls, "an unbound identity never reaches the handler")

	resp := callOK(t, fx.api, http.MethodDelete, fx.unbindPath(id, subject), "",
		http.StatusNotFound)
	require.Contains(t, resp, "identity_not_bound")
}

func TestIdentityRoutesDB_DuplicateIs409(t *testing.T) {
	fx := newIdentityFixture(t)
	first := createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)
	second := createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)
	subject := agentName("dup")
	callOK(t, fx.api, http.MethodPost, agentsPath+"/"+first+"/identities",
		fx.bindBody(subject), http.StatusCreated)
	for _, id := range []string{first, second} {
		resp := callOK(t, fx.api, http.MethodPost, agentsPath+"/"+id+"/identities",
			fx.bindBody(subject), http.StatusConflict)
		require.Contains(t, resp, "identity_bound")
	}
	callOK(t, fx.api, http.MethodPost, agentsPath+"/agp_zzzzzzzzzzzzzzzzzzzz/identities",
		fx.bindBody(agentName("nobody")), http.StatusNotFound)
}

// Two admins bind the same identity to two principals at once: one 201,
// one 409, and the identity resolves to the winner.
func TestIdentityRoutesDB_ConcurrentBindOneWins(t *testing.T) {
	fx := newIdentityFixture(t)
	other := fx.router(t, tokenRouteUser(t, fx.pool, auth.RoleAdmin))
	ids := []string{createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string),
		createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)}
	subject := agentName("race")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, h := range []http.Handler{fx.api, other} {
		wg.Add(1)
		go func(i int, h http.Handler) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, agentsPath+"/"+ids[i]+"/identities",
				strings.NewReader(fx.bindBody(subject)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			codes[i] = w.Code
		}(i, h)
	}
	wg.Wait()
	winner := -1
	for i, c := range codes {
		if c == http.StatusCreated {
			winner = i
		}
	}
	require.NotEqual(t, -1, winner, "codes %v", codes)
	require.Equal(t, http.StatusConflict, codes[1-winner], "codes %v", codes)
	code, body := fx.mcpCall(t, subject)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, ids[winner], fx.seen.identity.Principal.ID)
}

// Retiring a principal refuses its bound identity at once, and a retired
// or frozen principal gets no new binding.
//
// The plan expected agent_retired here; the resolver (which agentgov-03
// must not change) skips retired principals, so the real stack answers
// identity_unbound. Both are 403 and neither reaches the handler;
// agent_retired remains for a retirement between resolve and load.
func TestIdentityRoutesDB_RetiredAndFrozen(t *testing.T) {
	fx := newIdentityFixture(t)
	id := createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)
	subject := agentName("retiring")
	callOK(t, fx.api, http.MethodPost, agentsPath+"/"+id+"/identities",
		fx.bindBody(subject), http.StatusCreated)
	callOK(t, fx.api, http.MethodPatch, agentsPath+"/"+id,
		`{"status":"retired","reason":"replaced"}`, http.StatusOK)
	code, body := fx.mcpCall(t, subject)
	require.Equal(t, http.StatusForbidden, code, body)
	require.Contains(t, body, "identity_unbound")
	require.Equal(t, 0, fx.seen.calls)
	resp := callOK(t, fx.api, http.MethodPost, agentsPath+"/"+id+"/identities",
		fx.bindBody(agentName("late")), http.StatusConflict)
	require.Contains(t, resp, "retired")

	frozen := createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)
	_, err := agentguard.NewStore(fx.pool).SetStatus(context.Background(), frozen,
		agentguard.StatusFrozen, agentguard.Audit{Reason: "odd queries"})
	require.NoError(t, err)
	resp = callOK(t, fx.api, http.MethodPost, agentsPath+"/"+frozen+"/identities",
		fx.bindBody(agentName("frozen")), http.StatusConflict)
	require.Contains(t, resp, string(agentguard.ReasonFrozen))
}

// When the audit insert fails the bind answers 500 and no row exists.
func TestIdentityRoutesDB_RefusedAuditIs500AndBindsNothing(t *testing.T) {
	fx := newIdentityFixture(t)
	ctx := context.Background()
	id := createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)
	fn := strings.ReplaceAll(agentName("refuse_identity"), "-", "_")
	_, err := fx.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION sage.%s() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.detail->>'principal_id' = '%s' THEN RAISE EXCEPTION 'audit refused'; END IF;
		RETURN NEW; END $$`, fn, id))
	require.NoError(t, err)
	_, err = fx.pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON sage.auth_audit
		FOR EACH ROW EXECUTE FUNCTION sage.%s()`, fn, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+fn+
			" ON sage.auth_audit")
		_, _ = fx.pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS sage."+fn+"()")
	})
	subject := agentName("unaudited")
	resp := callOK(t, fx.api, http.MethodPost, agentsPath+"/"+id+"/identities",
		fx.bindBody(subject), http.StatusInternalServerError)
	require.NotContains(t, resp, "audit refused", "storage errors stay hidden")
	var n int
	require.NoError(t, fx.pool.QueryRow(ctx, `SELECT count(*) FROM
		sage.guard_identity_bindings WHERE subject = $1`, subject).Scan(&n))
	require.Equal(t, 0, n)
	code, _ := fx.mcpCall(t, subject)
	require.Equal(t, http.StatusForbidden, code)
}

func TestIdentityRoutesDB_OperatorIs403(t *testing.T) {
	fx := newIdentityFixture(t)
	id := createAgent(t, fx.api, fx.admin.ID, "dev")["id"].(string)
	subject := agentName("kept")
	callOK(t, fx.api, http.MethodPost, agentsPath+"/"+id+"/identities",
		fx.bindBody(subject), http.StatusCreated)
	op := fx.router(t, tokenRouteUser(t, fx.pool, auth.RoleOperator))
	callOK(t, op, http.MethodPost, agentsPath+"/"+id+"/identities",
		fx.bindBody(agentName("op")), http.StatusForbidden)
	callOK(t, op, http.MethodDelete, fx.unbindPath(id, subject), "", http.StatusForbidden)
	code, body := fx.mcpCall(t, subject)
	require.Equal(t, http.StatusOK, code, "the operator's unbind changed nothing: %s", body)
}
