package api

import (
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-05 review: PUT /api/v1/config (trust.level) writes the same global
// generation as PUT /api/v1/config/global. A durable conflict there must be
// a 409 that resyncs the losing replica, not a 500 that repeats until the
// next poll.
func TestLegacyConfigPutConflictResyncsTheLosingReplica(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetResyncGlobalConfig(t, ctx, pool)
	resetTrust := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND key = 'trust.level'`)
	}
	resetTrust()
	t.Cleanup(resetTrust)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	replicaA := newResyncReplica(cs, base)
	replicaB := newResyncReplica(cs, base)
	legacyPut := func(controller *config.ConfigController, body string) (int, map[string]any) {
		response := doRequestWithUser(configUpdateHandlerWithStore(controller, cs),
			http.MethodPut, "/api/v1/config", body, user)
		return response.Code, decodeResyncBody(t, response)
	}

	if code, body := putGlobal(t, replicaA, cs, user,
		`{"llm.timeout_seconds":77,"expected_generation":1}`); code != http.StatusOK {
		t.Fatalf("replica A PUT = %d %v", code, body)
	}
	code, body := legacyPut(replicaB,
		`{"trust":{"level":"observation"},"expected_generation":1}`)
	if code != http.StatusConflict || body["desired_generation"] != float64(2) {
		t.Fatalf("replica B stale PUT /api/v1/config = %d %v, want 409 at generation 2",
			code, body)
	}
	if got := replicaB.Active().Config.LLM.TimeoutSeconds; got != 77 {
		t.Fatalf("replica B timeout after the conflict = %d, want A's 77", got)
	}
	code, body = legacyPut(replicaB,
		`{"trust":{"level":"observation"},"expected_generation":2}`)
	if code != http.StatusOK || body["desired_generation"] != float64(3) {
		t.Fatalf("replica B retry = %d %v, want 200 at generation 3", code, body)
	}
	if generation, err := cs.GetGeneration(ctx, 0); err != nil || generation != 3 {
		t.Fatalf("durable generation = %d, %v; want 3", generation, err)
	}
}
