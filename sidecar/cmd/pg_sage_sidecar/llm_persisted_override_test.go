package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// Post-test audit: a "false" saved through the Settings UI/API lives in
// sage.config and must still win over the new default-on LLM switches
// when the process starts (initializeConfigController applies it).
func TestPersistedFalseOverridesBeatLLMDefaultsAtStartup(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	pool := preflightRuntimePool(t)
	ctx := context.Background()
	configs := store.NewConfigStore(pool)
	keys := map[string]func(*config.Config) bool{
		"llm.enabled":           func(c *config.Config) bool { return c.LLM.Enabled },
		"llm.optimizer.enabled": func(c *config.Config) bool { return c.LLM.Optimizer.Enabled },
		"advisor.enabled":       func(c *config.Config) bool { return c.Advisor.Enabled },
		"rca.narration_enabled": func(c *config.Config) bool { return c.RCA.NarrationEnabled },
		"explain.enabled":       func(c *config.Config) bool { return c.Explain.Enabled },
	}
	userID := overrideAuthor(t, pool)
	for key := range keys {
		if err := configs.SetOverride(ctx, key, "false", 0, userID); err != nil {
			t.Fatalf("persist %s=false: %v", key, err)
		}
		t.Cleanup(func() {
			if err := configs.DeleteOverride(ctx, key, 0); err != nil {
				t.Errorf("delete override %s: %v", key, err)
			}
		})
	}
	cfg = config.DefaultConfig()
	configController = nil
	out := captureStderr(t, func() {
		if err := initializeConfigController(pool); err != nil {
			t.Errorf("initializeConfigController: %v", err)
		}
	})
	for key, get := range keys {
		if get(cfg) {
			t.Errorf("%s: persisted false lost to the default", key)
		}
	}
	if strings.Contains(out, "llm.api_key") {
		t.Errorf("setup notice logged with llm.enabled persisted false: %q", out)
	}
}

// overrideAuthor creates the user an override is attributed to
// (sage.config.updated_by_user_id references sage.users). The package's
// test database is created and dropped per run.
func overrideAuthor(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.users
		(email, password, role) VALUES ($1, 'test-only-unusable-hash', 'admin')
		RETURNING id`, fmt.Sprintf("llm-defaults-%d@pg-sage.test",
		time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("create override author: %v", err)
	}
	return id
}

// tuner.llm_enabled is YAML-only: the override store refuses it, so the
// CHANGELOG points operators at the config file for that switch.
func TestTunerLLMSwitchIsNotAPersistedOverride(t *testing.T) {
	pool := preflightRuntimePool(t)
	err := store.NewConfigStore(pool).SetOverride(
		context.Background(), "tuner.llm_enabled", "false", 0, overrideAuthor(t, pool))
	if err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("SetOverride(tuner.llm_enabled) err = %v, want unknown key", err)
	}
}
