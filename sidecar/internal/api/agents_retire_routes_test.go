package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Retiring an agent records the admin who retired it: the leader drops its
// roles after agents.roles.retire_grace_days under that admin's approval.

func TestAgentRoutes_RetireRecordsTheRetiringAdmin(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	id := createAgent(t, h, admin.ID, "dev")["id"].(string)
	ctx := context.Background()
	readBy := func() *int {
		var by *int
		require.NoError(t, pool.QueryRow(ctx, `SELECT retired_by FROM sage.guard_principals
			WHERE id = $1`, id).Scan(&by))
		return by
	}
	require.Nil(t, readBy(), "an active agent has no retiring admin")

	// A narrowing patch without a status records nobody.
	code, body := tokenCall(t, h, http.MethodPatch, agentsPath+"/"+id, `{"env_ceiling":"branch"}`)
	require.Equal(t, http.StatusOK, code, body)
	require.Nil(t, readBy())

	code, body = tokenCall(t, h, http.MethodPatch, agentsPath+"/"+id, `{"status":"retired"}`)
	require.Equal(t, http.StatusOK, code, body)
	by := readBy()
	require.True(t, by != nil && *by == admin.ID, "retired_by = %v, want %d", by, admin.ID)

	// A second retire is refused and keeps the first admin.
	other := tokenRouteUser(t, pool, auth.RoleAdmin)
	h2 := agentRouter(t, pool, other, false)
	code, _ = tokenCall(t, h2, http.MethodPatch, agentsPath+"/"+id, `{"status":"retired"}`)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, admin.ID, *readBy())
}

func TestAgentRoutes_RetireUnknownAgent(t *testing.T) {
	pool := surfacePool(t)
	admin := tokenRouteUser(t, pool, auth.RoleAdmin)
	h := agentRouter(t, pool, admin, false)
	code, _ := tokenCall(t, h, http.MethodPatch, agentsPath+"/agp_aaaaaaaaaaaaaaaaaaaa",
		`{"status":"retired"}`)
	require.Equal(t, http.StatusNotFound, code)
}
