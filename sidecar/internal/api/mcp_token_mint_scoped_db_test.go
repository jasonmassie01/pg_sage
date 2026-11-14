package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// apiauth-08c: an admin minting a person's token for an owner scoped by
// grants may name only databases that owner may read; "*" or any other
// database is 400 database_forbidden and no token is stored.

func mintRouter(t *testing.T, pool *pgxpool.Pool, admin *auth.User, grantsOn bool,
	src DatabaseGrantSource) http.Handler {
	t.Helper()
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	cfg := config.DefaultConfig()
	cfg.Mode, cfg.API.DatabaseGrants = "fleet", grantsOn
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	runtime, err := mcp.NewRuntime(cfg.MCP, mcp.NewServer(&mcpAuthzBackend{}), nil, nil)
	require.NoError(t, err)
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, admin))
		})
	}
	mgr := grantsFleet("billing", "search")
	return newTestRouter(mgr, cfg, pool, &ActionDeps{Fleet: mgr}, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler(), DatabaseGrants: src}, inject)
}

func mintBody(name string, owner int, dbs string) string {
	return fmt.Sprintf(`{"name":%q,"kind":"operator","scopes":["read"],`+
		`"databases":%s,"expires_in_days":7,"owner_user_id":%d}`, name, dbs, owner)
}

func scopedOwner(t *testing.T, pool *pgxpool.Pool, admin int, role string,
	dbs ...string) *auth.User {
	t.Helper()
	owner := tokenRouteUser(t, pool, role)
	for _, db := range dbs {
		_, err := auth.NewDatabaseGrantStore(pool).SetGrant(context.Background(),
			auth.GrantChange{ActorID: admin, UserID: owner.ID, Database: db,
				Role: auth.RoleViewer})
		require.NoError(t, err)
	}
	return owner
}

func TestTokenMintScoped_OutsideTheOwnersGrantsIsForbidden(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := mintRouter(t, pool, admin, true, nil)
	owner := scopedOwner(t, pool, admin.ID, auth.RoleOperator, "billing")
	for _, dbs := range []string{`["search"]`, `["*"]`, `["billing","search"]`,
		`["nosuch"]`} {
		name := fmt.Sprintf("mint-refused-%d", time.Now().UnixNano())
		code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath,
			mintBody(name, owner.ID, dbs))
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", dbs, body)
		out := decodeObject(t, body)
		require.Equal(t, "database_forbidden", out["code"], "%s: %s", dbs, body)
		require.Contains(t, out["error"], "billing", "%s: the message names the grants", dbs)
		require.Equal(t, 0, countTokensNamed(t, pool, name), dbs)
	}
	name := fmt.Sprintf("mint-ok-%d", time.Now().UnixNano())
	code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath,
		mintBody(name, owner.ID, `["billing"]`))
	require.Equal(t, http.StatusCreated, code, body)
	require.Equal(t, []any{"billing"}, decodeObject(t, body)["databases"])
	require.Equal(t, 1, countTokensNamed(t, pool, name))
}

func TestTokenMintScoped_UnscopedOwnersAndSwitchOffAreUnchanged(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	on := mintRouter(t, pool, admin, true, nil)
	plain := tokenRouteUser(t, pool, auth.RoleOperator)
	adminOwner := scopedOwner(t, pool, admin.ID, auth.RoleAdmin, "billing")
	for _, owner := range []*auth.User{plain, adminOwner} {
		code, body := tokenCall(t, on, http.MethodPost, tokenRoutesPath,
			mintBody(fmt.Sprintf("mint-plain-%d", time.Now().UnixNano()), owner.ID, `["*"]`))
		require.Equal(t, http.StatusCreated, code, "%s owner: %s", owner.Role, body)
	}
	off := mintRouter(t, pool, admin, false, nil)
	scoped := scopedOwner(t, pool, admin.ID, auth.RoleOperator, "billing")
	code, body := tokenCall(t, off, http.MethodPost, tokenRoutesPath,
		mintBody(fmt.Sprintf("mint-off-%d", time.Now().UnixNano()), scoped.ID, `["*"]`))
	require.Equal(t, http.StatusCreated, code, "switch off: %s", body)
}

func TestTokenMintScoped_GrantStoreFailureRefusesTheMint(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	src := &dbAuthzSource{err: errors.New("connection refused")}
	h := mintRouter(t, pool, admin, true, src)
	owner := tokenRouteUser(t, pool, auth.RoleOperator)
	name := fmt.Sprintf("mint-503-%d", time.Now().UnixNano())
	code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath,
		mintBody(name, owner.ID, `["*"]`))
	require.Equal(t, http.StatusServiceUnavailable, code, body)
	require.Equal(t, "grant_store_unavailable", decodeObject(t, body)["code"])
	require.Equal(t, 0, countTokensNamed(t, pool, name))
}

// Invalid input: an unknown owner is the store's refusal (400, as today),
// not a grant decision; a malformed body is 400 before grants are read.
func TestTokenMintScoped_UnknownOwnerAndBadBodyKeepTheirErrors(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	src := &dbAuthzSource{}
	h := mintRouter(t, pool, admin, true, src)
	code, body := tokenCall(t, h, http.MethodPost, tokenRoutesPath,
		mintBody("mint-ghost", 987654321, `["billing"]`))
	require.Equal(t, http.StatusBadRequest, code, body)
	require.NotEqual(t, "database_forbidden", decodeObject(t, body)["code"])
	code, _ = tokenCall(t, h, http.MethodPost, tokenRoutesPath, `{"name":`)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, int64(0), src.calls.Load())
}
