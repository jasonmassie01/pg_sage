package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-03 on real PostgreSQL: GET /api/v1/config/global lists the
// overrides that mask an explicit file value (an empty array when none do)
// and shows the effective value, not the stored one, for a held-back trust
// raise.

func resetMaskingGlobalConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	reset := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config_audit
			WHERE database_id IS NULL AND key = 'trust.level'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND key IN
			('trust.level', '__pg_sage_config_generation')`)
	}
	reset()
	t.Cleanup(reset)
}

func getGlobalConfig(
	t *testing.T, controller *config.ConfigController, cs *store.ConfigStore,
	user *auth.User,
) map[string]any {
	t.Helper()
	response := doRequestWithUser(configGlobalGetHandler(cs, controller),
		http.MethodGet, "/api/v1/config/global", "", user)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", response.Code, response.Body.String())
	}
	return decodeResyncBody(t, response)
}

func TestConfigGlobalGetListsMaskedOverrides(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetMaskingGlobalConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}

	base := loadMaskingBase(t, "trust:\n  level: observation\n")
	empty := getGlobalConfig(t, config.NewConfigController(config.Clone(base), nil), cs, user)
	list, ok := empty["masked_by_override"].([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("masked_by_override = %#v, want an empty array", empty["masked_by_override"])
	}

	generation, err := cs.GetGeneration(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	writes := []store.ConfigOverrideWrite{{Key: "trust.level", Value: "autonomous"}}
	if _, err := cs.SetOverridesCAS(ctx, writes, 0, user.ID, generation); err != nil {
		t.Fatalf("store override: %v", err)
	}
	overrides, err := cs.GetOverrides(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	candidate := config.Clone(base)
	ReplayStoredOverrides(candidate, overrides, nil)
	body := getGlobalConfig(t, config.NewConfigController(candidate, nil), cs, user)
	list, ok = body["masked_by_override"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("masked_by_override = %#v, want one entry", body["masked_by_override"])
	}
	entry := list[0].(map[string]any)
	if entry["key"] != "trust.level" || entry["file_value"] != "observation" ||
		entry["override_value"] != "autonomous" || entry["effective_value"] != "observation" ||
		entry["updated_by"] != float64(user.ID) || entry["updated_at"] == "" {
		t.Fatalf("masked entry = %v", entry)
	}
	trust := body["config"].(map[string]any)["trust.level"].(map[string]any)
	if trust["value"] != "observation" || trust["source"] != "override" ||
		trust["masked"] != true {
		t.Fatalf("config trust.level = %v, want observation, override, masked", trust)
	}
}
