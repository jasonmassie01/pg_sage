package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// MCP bearer tokens on the real router with the REAL session middleware:
// a `Authorization: Bearer pgs_mcp_...` header authenticates the MCP
// endpoint (and only it) without a session cookie. The backend records
// what reaches it; tokens, store, schema and middleware are real.

type bearerCall struct{ tool, actor, database string }

type bearerBackend struct {
	mu    sync.Mutex
	calls []bearerCall
}

func (b *bearerBackend) record(tool, actor, database string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, bearerCall{tool: tool, actor: actor, database: database})
}

func (b *bearerBackend) snapshot() []bearerCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bearerCall(nil), b.calls...)
}

func (b *bearerBackend) GetPolicy(ctx context.Context, _ mcp.PolicyRequest,
) (mcp.PolicyResult, error) {
	b.record("get_policy", mcp.ActorFromContext(ctx), "")
	return mcp.PolicyResult{Version: 1, Profile: "staffed"}, nil
}

func (b *bearerBackend) ProposePolicyChange(ctx context.Context, _ mcp.PolicyProposalRequest,
) (mcp.PolicyProposalResult, error) {
	b.record("propose_policy_change", mcp.ActorFromContext(ctx), "")
	return mcp.PolicyProposalResult{ProposalID: 1}, nil
}

func (b *bearerBackend) RequestChange(ctx context.Context, _ mcp.ChangeRequest,
) (mcp.ChangeResult, error) {
	b.record("request_change", mcp.ActorFromContext(ctx), "")
	return mcp.ChangeResult{Decision: "parked"}, nil
}

func (b *bearerBackend) GetLedger(ctx context.Context, _ mcp.LedgerRequest,
) (mcp.LedgerResult, error) {
	b.record("get_ledger", mcp.ActorFromContext(ctx), "")
	return mcp.LedgerResult{}, nil
}

func (b *bearerBackend) ListFacts(ctx context.Context, req mcp.FactRequest) (any, error) {
	b.record("list_facts", mcp.ActorFromContext(ctx), req.Database)
	return map[string]any{"facts": []any{}}, nil
}

func (b *bearerBackend) ProposeFact(_ context.Context, req mcp.FactRequest,
	actor string) (any, error) {
	b.record("propose_fact", actor, req.Database)
	return map[string]any{"fact_id": 11, "status": "proposed"}, nil
}

func (b *bearerBackend) DecideFact(_ context.Context, req mcp.FactRequest,
	actor string) (any, error) {
	b.record("decide_fact", actor, req.Database)
	return map[string]any{"fact_id": req.FactID, "status": "confirmed"}, nil
}

type bearerDirectory struct{}

func (bearerDirectory) Databases() []mcp.DatabaseRef {
	return []mcp.DatabaseRef{{Name: "orders", ID: 0}, {Name: "billing", ID: 0}}
}

type bearerFixture struct {
	pool    *pgxpool.Pool
	store   *mcptoken.Store
	backend *bearerBackend
	h       http.Handler
}

func newBearerFixture(t *testing.T) *bearerFixture {
	t.Helper()
	pool := surfacePool(t)
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled = true
	cfg.MCP.Transport = "http"
	backend := &bearerBackend{}
	server := mcp.NewServer(backend).WithDirectory(bearerDirectory{})
	runtime, err := mcp.NewRuntime(cfg.MCP, server, nil, nil)
	require.NoError(t, err)
	h := NewRouterFullRuntime(nil, cfg, pool, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, SessionAuthMiddleware(pool))
	return &bearerFixture{pool: pool, store: mcptoken.NewStore(pool), backend: backend, h: h}
}

func (f *bearerFixture) agentToken(t *testing.T) mcptoken.Token {
	t.Helper()
	tok, err := f.store.Create(context.Background(), mcptoken.CreateRequest{
		Name: fmt.Sprintf("bearer-agent-%d", time.Now().UnixNano()),
		Kind: mcptoken.KindAgent, Scopes: []string{"read", "propose"},
		Databases: []string{"orders"}, ExpiresIn: 24 * time.Hour,
		CreatedBy: "admin@example.com",
	})
	require.NoError(t, err)
	return tok
}

func (f *bearerFixture) operatorToken(t *testing.T, role string) (mcptoken.Token, int) {
	t.Helper()
	owner := tokenRouteUser(t, f.pool, role)
	tok, err := f.store.Create(context.Background(), mcptoken.CreateRequest{
		Name: fmt.Sprintf("bearer-op-%d", time.Now().UnixNano()),
		Kind: mcptoken.KindOperator, Scopes: []string{"read", "propose", "approve"},
		Databases: []string{"*"}, ExpiresIn: 24 * time.Hour,
		OwnerUserID: owner.ID, CreatedBy: "admin@example.com",
	})
	require.NoError(t, err)
	return tok, owner.ID
}

func (f *bearerFixture) send(method, path, authz, cookie, body string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sage_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w
}

func (f *bearerFixture) callTool(t *testing.T, secret, tool, args string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
		`"params":{"name":%q,"arguments":%s}}`, tool, args)
	w := f.send(http.MethodPost, "/api/v1/mcp", "Bearer "+secret, "", body)
	return rpcResult(t, w)
}

func rpcResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	require.Nil(t, out["error"], "want a result, got protocol error: %s", w.Body.String())
	result, ok := out["result"].(map[string]any)
	require.True(t, ok, "missing result: %s", w.Body.String())
	return result
}

func requireToolOK(t *testing.T, result map[string]any) {
	t.Helper()
	isError, _ := result["isError"].(bool)
	require.False(t, isError, "tool failed: %v", result)
}

// requireToolError returns the structured error's reason and message.
func requireToolError(t *testing.T, result map[string]any, code int) (string, string) {
	t.Helper()
	require.Equal(t, true, result["isError"], "want a tool error: %v", result)
	structured, ok := result["structuredContent"].(map[string]any)
	require.True(t, ok, "missing structuredContent: %v", result)
	errObj, ok := structured["error"].(map[string]any)
	require.True(t, ok, "missing structuredContent.error: %v", result)
	require.Equal(t, float64(code), errObj["code"], "error: %v", errObj)
	reason, _ := errObj["reason"].(string)
	message, _ := errObj["message"].(string)
	return reason, message
}

const proposeFactArgs = `{"database":"orders","type":"test_fixture",` +
	`"subject_kind":"table","subject":"public.fixtures","evidence":"seeded by CI"}`

const decideFactArgs = `{"database":"orders","fact_id":11,"decision":"confirm"}`

func TestMCPBearerAgentReadsAndProposesOnPermittedDatabase(t *testing.T) {
	f := newBearerFixture(t)
	tok := f.agentToken(t)
	actor := "mcp:token:" + tok.ID

	requireToolOK(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"orders"}`))
	requireToolOK(t, f.callTool(t, tok.Secret, "propose_fact", proposeFactArgs))

	calls := f.backend.snapshot()
	require.Equal(t, []bearerCall{
		{tool: "list_facts", actor: actor, database: "orders"},
		{tool: "propose_fact", actor: actor, database: "orders"},
	}, calls)
	got, err := f.store.List(context.Background())
	require.NoError(t, err)
	for _, listed := range got {
		if listed.ID == tok.ID {
			require.NotNil(t, listed.LastUsedAt, "bearer use must record last_used_at")
			return
		}
	}
	t.Fatalf("token %s not listed", tok.ID)
}

func TestMCPBearerAgentCannotApprove(t *testing.T) {
	f := newBearerFixture(t)
	tok := f.agentToken(t)
	reason, _ := requireToolError(t,
		f.callTool(t, tok.Secret, "decide_fact", decideFactArgs), -32005)
	require.Equal(t, "approval_reserved_for_humans", reason)
	require.Empty(t, f.backend.snapshot(), "decide_fact must not reach the backend")
}

func TestMCPBearerDatabaseBinding(t *testing.T) {
	f := newBearerFixture(t)
	tok := f.agentToken(t)

	reasonB, msgB := requireToolError(t,
		f.callTool(t, tok.Secret, "list_facts", `{"database":"billing"}`), -32003)
	reasonN, msgN := requireToolError(t,
		f.callTool(t, tok.Secret, "list_facts", `{"database":"nosuch"}`), -32003)
	require.Equal(t, "database_not_permitted", reasonB)
	require.Equal(t, reasonB, reasonN, "no existence leak")
	require.Equal(t, strings.ReplaceAll(msgB, "billing", "nosuch"), msgN,
		"a missing database must read exactly like a forbidden one")

	reason, _ := requireToolError(t, f.callTool(t, tok.Secret, "list_facts", `{}`), -32006)
	require.Equal(t, "database_required", reason)
	require.Empty(t, f.backend.snapshot())
}

func TestMCPBearerToolsListMatchesScopes(t *testing.T) {
	f := newBearerFixture(t)
	tok := f.agentToken(t)
	w := f.send(http.MethodPost, "/api/v1/mcp", "Bearer "+tok.Secret, "",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	result := rpcResult(t, w)
	tools, ok := result["tools"].([]any)
	require.True(t, ok, "tools must be an array: %v", result)

	schemas := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		schema, _ := tool["inputSchema"].(map[string]any)
		schemas[tool["name"].(string)] = schema
	}
	require.Contains(t, schemas, "list_facts")
	require.Contains(t, schemas, "propose_fact")
	require.NotContains(t, schemas, "decide_fact")
	props, _ := schemas["list_facts"]["properties"].(map[string]any)
	database, _ := props["database"].(map[string]any)
	require.Equal(t, []any{"orders"}, database["enum"], "only permitted databases")
}

func TestMCPBearerRejectsBadCredentials(t *testing.T) {
	f := newBearerFixture(t)
	revoked := f.agentToken(t)
	_, err := f.store.Revoke(context.Background(), revoked.ID, "admin@example.com")
	require.NoError(t, err)
	expired := f.agentToken(t)
	_, err = f.pool.Exec(context.Background(), `UPDATE sage.mcp_tokens
		SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second'
		WHERE id::text = $1`, expired.ID)
	require.NoError(t, err)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"list_facts","arguments":{"database":"orders"}}}`
	cases := map[string]string{
		"no auth":       "",
		"garbage token": "Bearer pgs_mcp_garbage",
		"empty bearer":  "Bearer ",
		"revoked":       "Bearer " + revoked.Secret,
		"expired":       "Bearer " + expired.Secret,
	}
	for label, authz := range cases {
		w := f.send(http.MethodPost, "/api/v1/mcp", authz, "", body)
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s: %s", label, w.Body.String())
		require.NotContains(t, w.Body.String(), "pgs_mcp_", label)
	}
	require.Empty(t, f.backend.snapshot())
}

func TestMCPBearerInvalidDoesNotFallBackToSession(t *testing.T) {
	f := newBearerFixture(t)
	admin := tokenRouteUser(t, f.pool, auth.RoleAdmin)
	session, err := auth.CreateSession(context.Background(), f.pool, admin.ID)
	require.NoError(t, err)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"list_facts","arguments":{"database":"orders"}}}`

	// Control: the session alone is accepted.
	requireToolOK(t, rpcResult(t, f.send(http.MethodPost, "/api/v1/mcp", "", session, body)))
	require.Len(t, f.backend.snapshot(), 1)

	w := f.send(http.MethodPost, "/api/v1/mcp", "Bearer pgs_mcp_garbage", session, body)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Len(t, f.backend.snapshot(), 1, "an invalid bearer must not fall back")
}

func TestMCPBearerOperatorTokenFollowsOwner(t *testing.T) {
	f := newBearerFixture(t)
	tok, owner := f.operatorToken(t, auth.RoleOperator)

	requireToolOK(t, f.callTool(t, tok.Secret, "decide_fact", decideFactArgs))
	calls := f.backend.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, "decide_fact", calls[0].tool)
	require.Equal(t, "orders", calls[0].database)
	require.True(t, strings.HasPrefix(calls[0].actor, "mcp:token:"+tok.ID),
		"actor %q must name the token", calls[0].actor)

	_, err := f.pool.Exec(context.Background(),
		`UPDATE sage.users SET role = 'viewer' WHERE id = $1`, owner)
	require.NoError(t, err)
	requireToolError(t, f.callTool(t, tok.Secret, "decide_fact", decideFactArgs), -32001)
	requireToolOK(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"billing"}`))
	calls = f.backend.snapshot()
	require.Len(t, calls, 2, "the demoted owner's token reads but cannot decide")
	require.Equal(t, "list_facts", calls[1].tool)
	require.Equal(t, "billing", calls[1].database)
}

func TestMCPBearerOnlyAuthenticatesMCPEndpoint(t *testing.T) {
	f := newBearerFixture(t)
	tok, _ := f.operatorToken(t, auth.RoleAdmin)
	for _, path := range []string{"/api/v1/mcp/tokens", "/api/v1/users"} {
		w := f.send(http.MethodGet, path, "Bearer "+tok.Secret, "", "")
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s: %s", path, w.Body.String())
	}
	w := f.send(http.MethodGet, "/api/v1/mcp", "Bearer "+tok.Secret, "", "")
	require.Equal(t, http.StatusMethodNotAllowed, w.Code, w.Body.String())
	require.Empty(t, f.backend.snapshot())
}

func TestMCPBearerConcurrentCalls(t *testing.T) {
	f := newBearerFixture(t)
	tok := f.agentToken(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"list_facts","arguments":{"database":"orders"}}}`
	const n = 10
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.send(http.MethodPost, "/api/v1/mcp", "Bearer "+tok.Secret, "", body).Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		require.Equal(t, http.StatusOK, code, "request %d", i)
	}
	calls := f.backend.snapshot()
	require.Len(t, calls, n)
	for _, call := range calls {
		require.Equal(t, "mcp:token:"+tok.ID, call.actor)
	}
}
