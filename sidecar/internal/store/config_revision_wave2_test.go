package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestWave2ConfigRevisionValidatesEveryWriteBeforeOpeningTransaction(
	t *testing.T,
) {
	store := NewConfigStore(nil)
	err := store.SetOverrides(context.Background(), []ConfigOverrideWrite{
		{Key: "trust.level", Value: "advisory"},
		{Key: "unknown.wave2.key", Value: "value"},
	}, 0, 1)
	if err == nil || !strings.Contains(err.Error(), "unknown.wave2.key") {
		t.Fatalf("SetOverrides error = %v, want invalid second key", err)
	}
}

func TestWave2EmptyConfigRevisionIsANoop(t *testing.T) {
	store := NewConfigStore(nil)
	if err := store.SetOverrides(
		context.Background(), nil, 0, 1,
	); err != nil {
		t.Fatalf("empty SetOverrides: %v", err)
	}
}

func TestWave2SecretOverridesRemainMaskedInMergedResponse(t *testing.T) {
	merged := configToMap(config.DefaultConfig())
	applyOverrides(merged, []ConfigOverride{{
		Key: "llm.api_key", Value: "sk-plaintext-secret",
	}}, "override")
	entry := merged["llm.api_key"].(map[string]any)
	if got := entry["value"]; got == "sk-plaintext-secret" {
		t.Fatal("plaintext secret exposed by merged config")
	}
	if got := entry["source"]; got != "override" {
		t.Fatalf("source = %v, want override", got)
	}
}

func TestWave2AuditSecretValuesAreRedacted(t *testing.T) {
	oldValue, newValue := auditValues(
		"llm.api_key", "old-plaintext", "new-plaintext",
	)
	if oldValue == "old-plaintext" || newValue == "new-plaintext" {
		t.Fatalf("audit values leaked plaintext: %q %q", oldValue, newValue)
	}
	if oldValue != redactedSecret || newValue != redactedSecret {
		t.Fatalf("audit values = %q %q, want redacted", oldValue, newValue)
	}
}

func TestWave2GlobalConfigRevisionPersistsCASAndReset(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)
	keys := []string{"collector.batch_size", "safety.query_timeout_ms"}
	reset := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config_audit
			WHERE database_id IS NULL AND key = ANY($1)`, keys)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND (key = ANY($1) OR key = $2)`,
			keys, configGenerationKey)
	}
	reset()
	t.Cleanup(reset)

	if generation, err := cs.GetGeneration(ctx, 0); err != nil || generation != 1 {
		t.Fatalf("initial generation = %d, %v; want 1", generation, err)
	}
	generation, err := cs.SetOverridesCAS(ctx, []ConfigOverrideWrite{
		{Key: keys[0], Value: "250"},
		{Key: keys[1], Value: "4500"},
	}, 0, 99, 1)
	if err != nil || generation != 2 {
		t.Fatalf("set revision = %d, %v; want generation 2", generation, err)
	}
	if durable, err := cs.GetGeneration(ctx, 0); err != nil || durable != 2 {
		t.Fatalf("durable generation = %d, %v; want 2", durable, err)
	}
	if _, err := cs.SetOverridesCAS(ctx, []ConfigOverrideWrite{{
		Key: keys[0], Value: "300",
	}}, 0, 99, 1); !errors.Is(err, ErrConfigGenerationConflict) {
		t.Fatalf("stale revision error = %v, want conflict", err)
	}
	generation, err = cs.DeleteOverrideCAS(ctx, keys[0], 0, 99, 2)
	if err != nil || generation != 3 {
		t.Fatalf("reset revision = %d, %v; want generation 3", generation, err)
	}
	overrides, err := cs.GetOverrides(ctx, 0)
	if err != nil {
		t.Fatalf("get overrides: %v", err)
	}
	for _, override := range overrides {
		if override.Key == keys[0] {
			t.Fatalf("reset key %q remains with value %q", keys[0], override.Value)
		}
	}
}

func TestWave2DatabasePolicyRevisionIsAtomicAndAudited(t *testing.T) {
	pool, ctx := coverageDBWithUser(t)
	cs := NewConfigStore(pool)
	const databaseID = 9902
	reset := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.config_audit WHERE database_id = $1", databaseID)
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.config WHERE database_id = $1", databaseID)
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.databases WHERE id = $1", databaseID)
	}
	reset()
	t.Cleanup(reset)
	_, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(id, name, host, port, database_name, username, password_enc,
		 sslmode, trust_level, execution_mode)
		VALUES ($1, 'wave2-policy-revision', 'localhost', 5432, 'postgres',
		 'postgres', '\x00', 'disable', 'observation', 'approval')`, databaseID)
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}

	mode := "manual"
	generation, err := cs.SetDatabaseOverridesCAS(
		ctx, []ConfigOverrideWrite{{Key: "trust.level", Value: "advisory"}},
		databaseID, 99, 1, &mode,
	)
	if err != nil || generation != 2 {
		t.Fatalf("policy revision = %d, %v; want generation 2", generation, err)
	}
	var trust, execution string
	if err := pool.QueryRow(ctx, `SELECT trust_level, execution_mode
		FROM sage.databases WHERE id = $1`, databaseID).Scan(
		&trust, &execution,
	); err != nil {
		t.Fatalf("read database policy: %v", err)
	}
	if trust != "advisory" || execution != mode {
		t.Fatalf("policy = (%q, %q), want (advisory, manual)", trust, execution)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE database_id = $1 AND key IN ('trust.level', 'execution_mode')`,
		databaseID).Scan(&audits); err != nil {
		t.Fatalf("count policy audits: %v", err)
	}
	if audits != 2 {
		t.Fatalf("policy audits = %d, want 2", audits)
	}
	staleMode := "auto"
	if _, err := cs.SetDatabaseOverridesCAS(
		ctx, nil, databaseID, 99, 1, &staleMode,
	); !errors.Is(err, ErrConfigGenerationConflict) {
		t.Fatalf("stale policy error = %v, want conflict", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT execution_mode FROM sage.databases WHERE id = $1", databaseID,
	).Scan(&execution); err != nil || execution != mode {
		t.Fatalf("execution after stale write = %q, %v; want %q", execution, err, mode)
	}
}
