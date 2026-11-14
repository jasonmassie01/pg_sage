package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// apiauth-08c on PostgreSQL: the real router, the real MCP runtime and
// server over a fleet of billing, search and orders, real tokens and real
// grants (sage.user_database_grants). With api.database_grants on, a
// person's token reads only the databases its owner may read now.

type scopedDirectory struct{}

func (scopedDirectory) Databases() []mcp.DatabaseRef {
	return []mcp.DatabaseRef{{Name: "billing"}, {Name: "search"}, {Name: "orders"}}
}

type mcpScopedFixture struct {
	*bearerFixture
	admin int
}

// newMCPScopedFixture mounts MCP with grants on (fleet mode); src
// overrides the grant store, grantsOn false keeps today's behaviour.
func newMCPScopedFixture(t *testing.T, grantsOn bool, src DatabaseGrantSource,
) *mcpScopedFixture {
	t.Helper()
	pool := surfacePool(t)
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	cfg := config.DefaultConfig()
	cfg.Mode, cfg.API.DatabaseGrants = "fleet", grantsOn
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	backend := &bearerBackend{}
	server := mcp.NewServer(backend).WithDirectory(scopedDirectory{})
	runtime, err := mcp.NewRuntime(cfg.MCP, server, nil, nil)
	require.NoError(t, err)
	mgr := grantsFleet("billing", "search", "orders")
	h := newTestRouter(mgr, cfg, pool, &ActionDeps{Fleet: mgr}, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler(), DatabaseGrants: src},
		SessionAuthMiddleware(pool))
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	return &mcpScopedFixture{admin: admin.ID, bearerFixture: &bearerFixture{pool: pool,
		store: mcptoken.NewStore(pool), backend: backend, h: h}}
}

// personToken mints an operator token for a new operator owner.
func (f *mcpScopedFixture) personToken(t *testing.T, dbs ...string) (mcptoken.Token, int) {
	t.Helper()
	owner := tokenRouteUser(t, f.pool, auth.RoleOperator)
	tok, err := f.store.Create(context.Background(), mcptoken.CreateRequest{
		Name: fmt.Sprintf("scoped-%d", time.Now().UnixNano()),
		Kind: mcptoken.KindOperator, Scopes: []string{"read", "propose"},
		Databases: dbs, ExpiresIn: time.Hour, OwnerUserID: owner.ID,
		CreatedBy: "admin@example.com",
	})
	require.NoError(t, err)
	return tok, owner.ID
}

func (f *mcpScopedFixture) grant(t *testing.T, user int, dbs ...string) {
	t.Helper()
	store := auth.NewDatabaseGrantStore(f.pool)
	for _, db := range dbs {
		_, err := store.SetGrant(context.Background(), auth.GrantChange{ActorID: f.admin,
			UserID: user, Database: db, Role: auth.RoleViewer})
		require.NoError(t, err)
	}
}

func (f *mcpScopedFixture) revoke(t *testing.T, user int, db string) {
	t.Helper()
	require.NoError(t, auth.NewDatabaseGrantStore(f.pool).RevokeGrant(context.Background(),
		auth.GrantChange{ActorID: f.admin, UserID: user, Database: db}))
}

// listed is the sorted names list_databases returns for secret.
func (f *mcpScopedFixture) listed(t *testing.T, secret string) string {
	t.Helper()
	result := f.callTool(t, secret, "list_databases", `{}`)
	requireToolOK(t, result)
	structured, _ := result["structuredContent"].(map[string]any)
	rows, _ := structured["databases"].([]any)
	names := []string{}
	for _, row := range rows {
		m, _ := row.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func TestMCPScoped_StarTokenListsAndReadsOnlyGrantedDatabases(t *testing.T) {
	f := newMCPScopedFixture(t, true, nil)
	tok, owner := f.personToken(t, "*")
	f.grant(t, owner, "billing")

	require.Equal(t, "billing", f.listed(t, tok.Secret))
	requireToolOK(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"billing"}`))
	for _, db := range []string{"search", "nosuch"} {
		reason, msg := requireToolError(t, f.callTool(t, tok.Secret, "list_facts",
			fmt.Sprintf(`{"database":%q}`, db)), -32003)
		require.Equal(t, "database_not_permitted", reason, db)
		require.Contains(t, msg, db)
	}
	calls := f.backend.snapshot()
	require.Equal(t, 1, len(calls), "backend calls: %v", calls)
	require.Equal(t, "billing", calls[0].database)

	// The per-database endpoint of a database outside the grants: nothing.
	w := f.send(http.MethodPost, mcpDatabasePrefix+"search", "Bearer "+tok.Secret, "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_facts",`+
			`"arguments":{"database":"search"}}}`)
	requireToolError(t, rpcResult(t, w), -32003)
	require.Equal(t, 1, len(f.backend.snapshot()))
}

func TestMCPScoped_UnscopedOwnerAndSwitchOffAreUnchanged(t *testing.T) {
	f := newMCPScopedFixture(t, true, nil)
	tok, _ := f.personToken(t, "*")
	require.Equal(t, "billing,orders,search", f.listed(t, tok.Secret))
	requireToolOK(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"search"}`))

	off := newMCPScopedFixture(t, false, nil)
	scopedTok, owner := off.personToken(t, "*")
	off.grant(t, owner, "billing")
	require.Equal(t, "billing,orders,search", off.listed(t, scopedTok.Secret))
	requireToolOK(t, off.callTool(t, scopedTok.Secret, "list_facts", `{"database":"search"}`))
}

func TestMCPScoped_AgentTokenKeepsItsOwnList(t *testing.T) {
	f := newMCPScopedFixture(t, true, nil)
	tok := mintAgentToken(t, f.pool, []string{"search"})
	require.Equal(t, "search", f.listed(t, tok.Secret))
	requireToolOK(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"search"}`))
}

func TestMCPScoped_RevokeBetweenCallsNarrowsTheNextCall(t *testing.T) {
	f := newMCPScopedFixture(t, true, nil)
	tok, owner := f.personToken(t, "*")
	f.grant(t, owner, "billing", "search")
	require.Equal(t, "billing,search", f.listed(t, tok.Secret))
	requireToolOK(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"search"}`))

	f.revoke(t, owner, "search")
	require.Equal(t, "billing", f.listed(t, tok.Secret))
	requireToolError(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"search"}`),
		-32003)
}

func TestMCPScoped_GrantAddedAfterMintWidensUpToTheTokenList(t *testing.T) {
	f := newMCPScopedFixture(t, true, nil)
	tok, owner := f.personToken(t, "billing", "search")
	f.grant(t, owner, "billing")
	require.Equal(t, "billing", f.listed(t, tok.Secret))
	f.grant(t, owner, "search")
	require.Equal(t, "billing,search", f.listed(t, tok.Secret))
	f.grant(t, owner, "orders")
	require.Equal(t, "billing,search", f.listed(t, tok.Secret))
	requireToolError(t, f.callTool(t, tok.Secret, "list_facts", `{"database":"orders"}`),
		-32003)
}

func TestMCPScoped_GrantStoreFailureIsUnavailable(t *testing.T) {
	src := &dbAuthzSource{err: errors.New("connection refused")}
	f := newMCPScopedFixture(t, true, src)
	tok, _ := f.personToken(t, "*")
	for _, call := range [][2]string{{"list_databases", `{}`},
		{"list_facts", `{"database":"billing"}`}} {
		reason, msg := requireToolError(t, f.callTool(t, tok.Secret, call[0], call[1]),
			-32010)
		require.Equal(t, "unavailable", reason, call[0])
		require.NotContains(t, msg, "connection refused")
	}
	require.Equal(t, 0, len(f.backend.snapshot()))
	require.True(t, src.calls.Load() >= 2, "grant store read %d times", src.calls.Load())
}

func TestMCPScoped_ParallelCallsOfTwoOwners(t *testing.T) {
	f := newMCPScopedFixture(t, true, nil)
	billingTok, billingOwner := f.personToken(t, "*")
	searchTok, searchOwner := f.personToken(t, "*")
	f.grant(t, billingOwner, "billing")
	f.grant(t, searchOwner, "search")
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for i := 0; i < 40; i++ {
		secret, want := billingTok.Secret, "billing"
		if i%2 == 1 {
			secret, want = searchTok.Secret, "search"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := scopedListed(f, secret); got != want {
				errs <- fmt.Sprintf("listed %q, want %q", got, want)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// scopedListed is listed without t (safe in goroutines): "" on failure.
func scopedListed(f *mcpScopedFixture, secret string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"list_databases","arguments":{}}}`
	w := f.send(http.MethodPost, mcpEndpointPath, "Bearer "+secret, "", body)
	if w.Code != http.StatusOK {
		return fmt.Sprintf("status %d", w.Code)
	}
	var names []string
	for _, db := range []string{"billing", "search", "orders"} {
		if strings.Contains(w.Body.String(), `"name":"`+db+`"`) {
			names = append(names, db)
		}
	}
	return strings.Join(names, ",")
}
