package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-05 on real PostgreSQL: two replicas (controllers) share one
// control database. A conflict makes the losing replica resync, so the
// UI's refetch-and-retry succeeds instead of answering 409 forever.

type resyncLLMOwner struct{}

func (resyncLLMOwner) Name() string { return config.LLMOwner }

func (resyncLLMOwner) Prepare(
	context.Context, config.ConfigSnapshot, config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	return apiPreparedConfig{}, nil
}

// newResyncReplica is one sidecar's controller wired to the shared store.
func newResyncReplica(cs *store.ConfigStore, base *config.Config) *config.ConfigController {
	controller := config.NewConfigController(base, nil, resyncLLMOwner{})
	controller.SetDurableSource(GlobalDurableSource{
		Store: cs, Base: staticConfigBase(base),
	})
	return controller
}

func resetResyncGlobalConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	reset := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config_audit
			WHERE database_id IS NULL AND key = 'llm.timeout_seconds'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND key IN
			('llm.timeout_seconds', '__pg_sage_config_generation')`)
	}
	reset()
	t.Cleanup(reset)
}

func putGlobal(
	t *testing.T, controller *config.ConfigController, cs *store.ConfigStore,
	user *auth.User, body string,
) (int, map[string]any) {
	t.Helper()
	response := doRequestWithUser(configGlobalPutHandler(cs, controller),
		http.MethodPut, "/api/v1/config/global", body, user)
	return response.Code, decodeResyncBody(t, response)
}

func deleteGlobal(
	t *testing.T, controller *config.ConfigController, cs *store.ConfigStore,
	base *config.Config, user *auth.User, query string,
) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/v1/config/global/{key}",
		configGlobalDeleteHandler(cs, staticConfigBase(base), controller))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete,
		"/api/v1/config/global/llm.timeout_seconds?"+query, nil)
	mux.ServeHTTP(response, withUser(request, user))
	return response.Code, decodeResyncBody(t, response)
}

func decodeResyncBody(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response (status %d): %v", response.Code, err)
	}
	return body
}

func TestConfigConflictResyncsSoRetryWithFreshGenerationSucceeds(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetResyncGlobalConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	replicaA := newResyncReplica(cs, base)
	replicaB := newResyncReplica(cs, base)

	code, body := putGlobal(t, replicaA, cs, user,
		`{"llm.timeout_seconds":77,"expected_generation":1}`)
	if code != http.StatusOK || body["desired_generation"] != float64(2) {
		t.Fatalf("replica A PUT = %d %v, want 200 at generation 2", code, body)
	}
	code, body = putGlobal(t, replicaB, cs, user,
		`{"llm.timeout_seconds":88,"expected_generation":1}`)
	if code != http.StatusConflict {
		t.Fatalf("replica B stale PUT = %d %v, want 409", code, body)
	}
	if body["desired_generation"] != float64(2) || body["error"] == nil {
		t.Fatalf("409 body = %v, want the fresh desired_generation 2 and an error", body)
	}
	if got := replicaB.Active(); got.Generation != 2 || got.Config.LLM.TimeoutSeconds != 77 {
		t.Fatalf("replica B after conflict = generation %d timeout %d, want 2 and 77",
			got.Generation, got.Config.LLM.TimeoutSeconds)
	}

	get := httptest.NewRecorder()
	configGlobalGetHandler(cs, replicaB).ServeHTTP(get,
		httptest.NewRequest(http.MethodGet, "/api/v1/config/global", nil))
	getBody := decodeResyncBody(t, get)
	if getBody["desired_generation"] != float64(2) || getBody["last_resync_at"] == nil {
		t.Fatalf("replica B GET = %v, want generation 2 and last_resync_at", getBody)
	}
	code, body = putGlobal(t, replicaB, cs, user,
		`{"llm.timeout_seconds":88,"expected_generation":2}`)
	if code != http.StatusOK || body["desired_generation"] != float64(3) {
		t.Fatalf("replica B retry = %d %v, want 200 at generation 3", code, body)
	}

	// Replica A's poll adopts B's revision.
	if _, adopted, err := replicaA.ResyncFromDurable(ctx); err != nil || !adopted {
		t.Fatalf("replica A resync = adopted %v, err %v", adopted, err)
	}
	if got := replicaA.Active(); got.Generation != 3 || got.Config.LLM.TimeoutSeconds != 88 {
		t.Fatalf("replica A after resync = generation %d timeout %d, want 3 and 88",
			got.Generation, got.Config.LLM.TimeoutSeconds)
	}
}

func TestConfigDeleteConflictResyncsTheLosingReplica(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetResyncGlobalConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	replicaA := newResyncReplica(cs, base)
	replicaB := newResyncReplica(cs, base)

	if code, body := putGlobal(t, replicaA, cs, user,
		`{"llm.timeout_seconds":77,"expected_generation":1}`); code != http.StatusOK {
		t.Fatalf("replica A PUT = %d %v", code, body)
	}
	code, body := deleteGlobal(t, replicaB, cs, base, user, "expected_generation=1")
	if code != http.StatusConflict || body["desired_generation"] != float64(2) {
		t.Fatalf("replica B stale DELETE = %d %v, want 409 at generation 2", code, body)
	}
	code, body = deleteGlobal(t, replicaB, cs, base, user, "expected_generation=2")
	if code != http.StatusOK || body["desired_generation"] != float64(3) {
		t.Fatalf("replica B DELETE retry = %d %v, want 200 at generation 3", code, body)
	}
	if got := replicaB.Active().Config.LLM.TimeoutSeconds; got != config.DefaultLLMTimeoutSeconds {
		t.Fatalf("replica B timeout after delete = %d, want the default %d",
			got, config.DefaultLLMTimeoutSeconds)
	}
}

func TestGlobalDurableSourceReadsGenerationAndOverrides(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetResyncGlobalConfig(t, ctx, pool)
	source := GlobalDurableSource{Store: cs, Base: staticConfigBase(config.DefaultConfig())}

	if generation, err := source.DurableGeneration(ctx); err != nil || generation != 1 {
		t.Fatalf("fresh durable generation = %d, %v; want 1", generation, err)
	}
	writes := []store.ConfigOverrideWrite{{Key: "llm.timeout_seconds", Value: "66"}}
	if _, err := cs.SetOverridesCAS(ctx, writes, 0, 0, 1); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	generation, err := source.DurableGeneration(ctx)
	if err != nil || generation != 2 {
		t.Fatalf("durable generation after a write = %d, %v; want 2", generation, err)
	}
	candidate, err := source.DurableCandidate(ctx)
	if err != nil || candidate.LLM.TimeoutSeconds != 66 {
		t.Fatalf("durable candidate = %+v, %v; want timeout 66", candidate, err)
	}
}
