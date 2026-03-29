//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

func setupConfigTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_TEST_DSN")
	if dsn == "" {
		dsn = testDSN()
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to test db: %v", err)
	}

	if err := schema.Bootstrap(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("bootstrapping schema: %v", err)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrating config schema: %v", err)
	}

	// Seed a test user so FK on updated_by_user_id is satisfied.
	_, err = pool.Exec(ctx, `
		INSERT INTO sage.users (id, email, password, role)
		VALUES (1, 'test@test.com', 'hashed', 'admin')
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		pool.Close()
		t.Fatalf("seeding test user: %v", err)
	}

	t.Cleanup(func() {
		cleanupConfig(pool)
		schema.ReleaseAdvisoryLock(context.Background(), pool)
		pool.Close()
	})

	return pool
}

func cleanupConfig(pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second)
	defer cancel()
	pool.Exec(ctx,
		"DELETE FROM sage.config_audit WHERE key LIKE 'test.%'")
	pool.Exec(ctx,
		"DELETE FROM sage.config WHERE key LIKE 'test.%'")
	// Remove config rows that reference the seeded test user,
	// then remove the test user itself to avoid FK conflicts
	// with other packages' tests.
	pool.Exec(ctx,
		"UPDATE sage.config SET updated_by_user_id = NULL "+
			"WHERE updated_by_user_id = 1")
	pool.Exec(ctx,
		"DELETE FROM sage.users WHERE id = 1 AND email = 'test@test.com'")
}

func TestConfigStoreSetAndGet(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()

	// Set a global override.
	err := cs.SetOverride(ctx,
		"collector.interval_seconds", "120", 0, 1)
	if err != nil {
		t.Fatalf("SetOverride global: %v", err)
	}

	// Read it back.
	overrides, err := cs.GetOverrides(ctx, 0)
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}

	found := false
	for _, o := range overrides {
		if o.Key == "collector.interval_seconds" {
			found = true
			if o.Value != "120" {
				t.Errorf("value = %q, want 120", o.Value)
			}
			if o.DatabaseID != 0 {
				t.Errorf("database_id = %d, want 0", o.DatabaseID)
			}
		}
	}
	if !found {
		t.Error("override not found in GetOverrides")
	}

	// Clean up.
	err = cs.DeleteOverride(ctx, "collector.interval_seconds", 0)
	if err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
}

func TestConfigStorePerDB(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()

	// Set global + per-DB.
	if err := cs.SetOverride(ctx,
		"collector.interval_seconds", "60", 0, 1); err != nil {
		t.Fatalf("SetOverride global: %v", err)
	}
	if err := cs.SetOverride(ctx,
		"collector.interval_seconds", "30", 1, 1); err != nil {
		t.Fatalf("SetOverride db: %v", err)
	}

	// Per-DB overrides should be separate from global.
	dbOverrides, err := cs.GetOverrides(ctx, 1)
	if err != nil {
		t.Fatalf("GetOverrides db: %v", err)
	}
	found := false
	for _, o := range dbOverrides {
		if o.Key == "collector.interval_seconds" {
			found = true
			if o.Value != "30" {
				t.Errorf("db value = %q, want 30", o.Value)
			}
		}
	}
	if !found {
		t.Error("per-DB override not found")
	}

	// Clean up.
	cs.DeleteOverride(ctx, "collector.interval_seconds", 0)
	cs.DeleteOverride(ctx, "collector.interval_seconds", 1)
}

func TestConfigStoreDeleteOverride(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()

	cs.SetOverride(ctx, "collector.batch_size", "500", 0, 1)

	err := cs.DeleteOverride(ctx, "collector.batch_size", 0)
	if err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}

	overrides, _ := cs.GetOverrides(ctx, 0)
	for _, o := range overrides {
		if o.Key == "collector.batch_size" {
			t.Error("override still exists after delete")
		}
	}
}

func TestConfigStoreAuditLogging(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()

	// Two changes to generate audit entries.
	cs.SetOverride(ctx, "collector.interval_seconds", "60", 0, 1)
	cs.SetOverride(ctx, "collector.interval_seconds", "120", 0, 1)

	entries, err := cs.GetAuditLog(ctx, 10)
	if err != nil {
		t.Fatalf("GetAuditLog: %v", err)
	}

	// Should have at least 2 entries.
	count := 0
	for _, e := range entries {
		if e.Key == "collector.interval_seconds" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("expected >= 2 audit entries, got %d", count)
	}

	// Clean up.
	cs.DeleteOverride(ctx, "collector.interval_seconds", 0)
}

func TestConfigStoreValidation(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()

	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"bad key", "invalid.key", "42", true},
		{"bad int value", "collector.interval_seconds", "abc", true},
		{"int too low", "collector.interval_seconds", "2", true},
		{"bad trust", "trust.level", "yolo", true},
		{"good trust", "trust.level", "advisory", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cs.SetOverride(ctx, tt.key, tt.value, 0, 1)
			if (err != nil) != tt.wantErr {
				t.Errorf("SetOverride(%q,%q) err=%v, wantErr=%v",
					tt.key, tt.value, err, tt.wantErr)
			}
		})
	}

	// Clean up any successful overrides.
	cs.DeleteOverride(ctx, "trust.level", 0)
}

func TestConfigStoreMergedConfig(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()

	// Load default config for merging tests.
	cfg, err := config.Load(
		[]string{"-mode", "standalone", "-pg-host", "localhost"})
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}

	// No overrides: should return yaml-sourced values.
	merged, err := cs.GetMergedConfig(ctx, cfg, 0)
	if err != nil {
		t.Fatalf("GetMergedConfig: %v", err)
	}
	entry, ok := merged["collector.interval_seconds"]
	if !ok {
		t.Fatal("missing collector.interval_seconds in merged")
	}
	em, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("unexpected entry type: %T", entry)
	}
	if em["source"] != "yaml" {
		t.Errorf("source = %v, want yaml", em["source"])
	}

	// Set global override.
	cs.SetOverride(ctx, "collector.interval_seconds", "120", 0, 1)

	merged, _ = cs.GetMergedConfig(ctx, cfg, 0)
	em = merged["collector.interval_seconds"].(map[string]any)
	if em["source"] != "override" {
		t.Errorf("source = %v, want override", em["source"])
	}
	if fmt.Sprintf("%v", em["value"]) != "120" {
		t.Errorf("value = %v, want 120", em["value"])
	}

	// Set per-DB override — should win over global.
	cs.SetOverride(ctx, "collector.interval_seconds", "30", 1, 1)
	merged, _ = cs.GetMergedConfig(ctx, cfg, 1)
	em = merged["collector.interval_seconds"].(map[string]any)
	if em["source"] != "db_override" {
		t.Errorf("source = %v, want db_override", em["source"])
	}
	if fmt.Sprintf("%v", em["value"]) != "30" {
		t.Errorf("value = %v, want 30", em["value"])
	}

	// Clean up.
	cs.DeleteOverride(ctx, "collector.interval_seconds", 0)
	cs.DeleteOverride(ctx, "collector.interval_seconds", 1)
}
