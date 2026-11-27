package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// cli-01b: ?overrides=1 on PostgreSQL with real stored overrides, a
// secret one included. Run on PG14 and PG17.

func yamlStorePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "api_cfg_yaml"))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func TestConfigYAMLDB_OnlyOverrides(t *testing.T) {
	pool := yamlStorePool(t)
	ctx := context.Background()
	cs := store.NewConfigStore(pool)
	cfg := config.DefaultConfig()
	h := configYAMLHandler(cs, config.NewConfigController(cfg, nil))
	get := func(query string) map[string]any {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/api/v1/config/yaml"+query, nil))
		var out map[string]any
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
		return out
	}
	if out := get("?overrides=1"); out["yaml"] != "{}\n" {
		t.Fatalf("no overrides: %v; want {}", out)
	}
	if err := cs.SetOverride(ctx, "collector.batch_size", "250", 0, 0); err != nil {
		t.Fatalf("override: %v", err)
	}
	if err := cs.SetOverride(ctx, "llm.api_key", yamlFixtureSecret, 0, 0); err != nil {
		t.Fatalf("secret override: %v", err)
	}
	// The runtime replays overrides into the desired config.
	cfg.Collector.BatchSize = 250
	cfg.LLM.APIKey = yamlFixtureSecret
	h = configYAMLHandler(cs, config.NewConfigController(cfg, nil))
	out := get("?overrides=1")
	raw, _ := json.Marshal(out)
	want := "collector:\n    batch_size: 250\nllm:\n    api_key: ${SAGE_LLM_API_KEY}\n"
	if out["yaml"] != want || strings.Contains(string(raw), yamlFixtureSecret) {
		t.Fatalf("overrides: %s; want yaml %q and no secret", raw, want)
	}
	keys, _ := out["keys"].([]any)
	if len(keys) != 2 || keys[0] != "collector.batch_size" || keys[1] != "llm.api_key" {
		t.Fatalf("keys %v", out["keys"])
	}
	if err := cs.DeleteOverride(ctx, "collector.batch_size", 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if keys, _ := get("?overrides=1")["keys"].([]any); len(keys) != 1 ||
		keys[0] != "llm.api_key" {
		t.Fatalf("after delete: keys %v", keys)
	}
}

// Error propagation: a store that cannot be read is a 500 with a message,
// never a partial or empty export that reads as "no overrides".
func TestConfigYAMLDB_StoreDownIs500(t *testing.T) {
	pool := yamlStorePool(t)
	cs := store.NewConfigStore(pool)
	pool.Close()
	h := configYAMLHandler(cs, config.NewConfigController(config.DefaultConfig(), nil))
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/api/v1/config/yaml?overrides=1", nil))
	if w.Code != http.StatusInternalServerError ||
		!strings.Contains(w.Body.String(), "failed to load stored overrides") {
		t.Fatalf("store down: %d %s", w.Code, w.Body.String())
	}
}
