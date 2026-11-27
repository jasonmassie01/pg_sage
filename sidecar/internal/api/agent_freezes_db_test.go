package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-07 on the real router and PostgreSQL: GET /api/v1/agents/freezes
// lists the open freezes of the control database (never a cleared one) for
// operators and admins, and GET /api/v1/agents names the retire grace the
// drawer's retire dialog shows. Database targets are the test's own: a
// fleet flag would freeze every agent change other tests make.

func freezesRouter(t *testing.T, pool *pgxpool.Pool, user *auth.User) http.Handler {
	t.Helper()
	sw, err := agentguard.NewSwitch(agentguard.KillDeps{Store: agentguard.NewStore(pool),
		Config:  agentguard.DefaultKillConfig(),
		Targets: func(context.Context) ([]agentguard.KillTarget, error) { return nil, nil }})
	require.NoError(t, err)
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			next.ServeHTTP(w, r)
		})
	}
	return newTestRouter(nil, config.DefaultConfig(), pool, nil, nil, nil,
		&RuntimeDeps{AgentKill: sw}, inject)
}

func insertDatabaseFreeze(t *testing.T, pool *pgxpool.Pool, target string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO sage.guard_freezes
		(scope, target, reason, set_by) VALUES ('database', $1, 'route drill', 'a@x')
		RETURNING id`, target).Scan(&id))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE sage.guard_freezes
			SET cleared_at = now(), cleared_by = 'test cleanup'
			WHERE id = $1 AND cleared_at IS NULL`, id)
	})
	return id
}

func TestAgentFreezesRoute_RealStore(t *testing.T) {
	pool := surfacePool(t)
	open := fmt.Sprintf("fz-route-%d", time.Now().UnixNano())
	gone := open + "-cleared"
	insertDatabaseFreeze(t, pool, open)
	clearedID := insertDatabaseFreeze(t, pool, gone)
	_, err := pool.Exec(context.Background(), `UPDATE sage.guard_freezes
		SET cleared_at = now(), cleared_by = 'b@x' WHERE id = $1`, clearedID)
	require.NoError(t, err)

	op := tokenRouteUser(t, pool, auth.RoleOperator)
	resp := callOK(t, freezesRouter(t, pool, op), http.MethodGet, freezesPath, "",
		http.StatusOK)
	got := decodeObject(t, resp)
	items, ok := got["freezes"].([]any)
	require.True(t, ok, "freezes array in %s", resp)
	var seen, seenCleared bool
	for _, it := range items {
		f := it.(map[string]any)
		seen = seen || f["target"] == open
		seenCleared = seenCleared || f["target"] == gone
	}
	require.True(t, seen, "the open freeze is listed: %s", resp)
	require.False(t, seenCleared, "a cleared freeze is not: %s", resp)

	viewer := tokenRouteUser(t, pool, auth.RoleViewer)
	callOK(t, freezesRouter(t, pool, viewer), http.MethodGet, freezesPath, "",
		http.StatusForbidden)
}

func TestAgentListRoute_NamesTheRetireGrace(t *testing.T) {
	pool := surfacePool(t)
	op := tokenRouteUser(t, pool, auth.RoleOperator)
	resp := callOK(t, agentRouter(t, pool, op, false), http.MethodGet, agentsPath, "",
		http.StatusOK)
	got := decodeObject(t, resp)
	require.Equal(t, float64(config.DefaultAgentRetireGraceDays), got["retire_grace_days"],
		resp)
}
