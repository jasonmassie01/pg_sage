//go:build integration

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// CG-01 / E1-01: secrets set through the API are sealed at rest under
// encryption_key with a key id; rotation re-seals them.

const fixtureAPIKey = "sk-fixture-not-a-real-key-0001"

func keyringOf(t *testing.T, active byte, previous ...byte) *crypto.Keyring {
	t.Helper()
	var prev [][]byte
	for _, b := range previous {
		prev = append(prev, bytes.Repeat([]byte{b}, 32))
	}
	kr, err := crypto.NewKeyring(bytes.Repeat([]byte{active}, 32), prev...)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func rawConfigValue(t *testing.T, pool *pgxpool.Pool, key string, dbID int) string {
	t.Helper()
	var v string
	err := pool.QueryRow(context.Background(),
		`SELECT value FROM sage.config WHERE key = $1 AND COALESCE(database_id, 0) = $2`,
		key, dbID).Scan(&v)
	if err != nil {
		t.Fatalf("raw %s: %v", key, err)
	}
	return v
}

func overrideValue(t *testing.T, cs *ConfigStore, key string) (string, bool) {
	t.Helper()
	overrides, err := cs.GetOverrides(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetOverrides: %v", err)
	}
	for _, o := range overrides {
		if o.Key == key {
			return o.Value, true
		}
	}
	return "", false
}

func cleanupSecretRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.config WHERE key IN ('llm.api_key', 'clone.dle_token',
			 'alerting.slack_webhook_url', 'alerting.pagerduty_routing_key',
			 'briefing.slack_webhook_url', 'llm.timeout_seconds')`)
	}
	clean()
	t.Cleanup(clean)
}

func TestE1_01_APIKeyIsCiphertextAtRest(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	kr := keyringOf(t, 1)
	cs := NewConfigStore(pool).WithKeyring(kr)
	if err := cs.SetOverride(context.Background(), "llm.api_key", fixtureAPIKey, 0, 1); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	raw := rawConfigValue(t, pool, "llm.api_key", 0)
	if raw == fixtureAPIKey || strings.Contains(raw, fixtureAPIKey) {
		t.Fatalf("E1-01: sage.config holds the plaintext key: %q", raw)
	}
	if !crypto.IsSealed(raw) {
		t.Fatalf("E1-01: stored value %q is not sealed", raw)
	}
	if kid, _ := crypto.SealedKeyID(raw); kid != kr.ActiveKeyID() {
		t.Fatalf("stored key id %q, want active %q", kid, kr.ActiveKeyID())
	}
	if got, ok := overrideValue(t, cs, "llm.api_key"); !ok || got != fixtureAPIKey {
		t.Fatalf("GetOverrides = (%q, %v), want the plaintext", got, ok)
	}
}

func TestConfigSecrets_NonSecretKeysStayPlaintext(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	cs := NewConfigStore(pool).WithKeyring(keyringOf(t, 1))
	if err := cs.SetOverride(context.Background(), "llm.timeout_seconds", "45", 0, 1); err != nil {
		t.Fatal(err)
	}
	if raw := rawConfigValue(t, pool, "llm.timeout_seconds", 0); raw != "45" {
		t.Fatalf("non-secret stored as %q, want 45", raw)
	}
}

func TestConfigSecrets_EverySecretKeyIsSealed(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	cs := NewConfigStore(pool).WithKeyring(keyringOf(t, 1))
	values := map[string]string{
		"llm.api_key":                    fixtureAPIKey,
		"clone.dle_token":                "fixture-dle-token",
		"alerting.slack_webhook_url":     "https://hooks.slack.com/services/T0/B0/fixture",
		"alerting.pagerduty_routing_key": "fixture-routing-key-000000000000",
	}
	for k, v := range values {
		if err := cs.SetOverride(context.Background(), k, v, 0, 1); err != nil {
			t.Fatalf("SetOverride %s: %v", k, err)
		}
		if raw := rawConfigValue(t, pool, k, 0); !crypto.IsSealed(raw) {
			t.Fatalf("%s stored unsealed", k)
		}
		if got, _ := overrideValue(t, cs, k); got != v {
			t.Fatalf("%s read back %q", k, got)
		}
	}
}

func TestConfigSecrets_NoKeyKeepsPlaintext(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	cs := NewConfigStore(pool).WithKeyring(nil)
	if err := cs.SetOverride(context.Background(), "llm.api_key", fixtureAPIKey, 0, 1); err != nil {
		t.Fatal(err)
	}
	if raw := rawConfigValue(t, pool, "llm.api_key", 0); raw != fixtureAPIKey {
		t.Fatalf("without a key the value must stay as today; got %q", raw)
	}
	if got, _ := overrideValue(t, cs, "llm.api_key"); got != fixtureAPIKey {
		t.Fatalf("read back %q", got)
	}
}

func TestConfigSecrets_ClientCannotWriteSealedLookingValue(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	for _, kr := range []*crypto.Keyring{nil, keyringOf(t, 1)} {
		cs := NewConfigStore(pool).WithKeyring(kr)
		err := cs.SetOverride(context.Background(), "llm.api_key",
			crypto.SealedPrefix+"0000000000000000:AAAA", 0, 1)
		if !errors.Is(err, ErrSealedValueRejected) {
			t.Fatalf("keyring %v: err = %v, want ErrSealedValueRejected", kr != nil, err)
		}
	}
}

func TestConfigSecrets_StartupSealsPlaintextRows(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	plain := NewConfigStore(pool).WithKeyring(nil)
	ctx := context.Background()
	_ = plain.SetOverride(ctx, "llm.api_key", fixtureAPIKey, 0, 1)
	_ = plain.SetOverride(ctx, "clone.dle_token", "fixture-dle-token", 0, 1)

	report, err := plain.SealStoredSecrets(ctx)
	if err != nil || report.Plaintext != 2 || report.Sealed != 0 {
		t.Fatalf("no-key report = %+v, %v; want 2 plaintext, 0 sealed", report, err)
	}
	kr := keyringOf(t, 1)
	sealed := NewConfigStore(pool).WithKeyring(kr)
	report, err = sealed.SealStoredSecrets(ctx)
	if err != nil || report.Sealed != 2 || report.Rotated != 0 || report.Plaintext != 0 {
		t.Fatalf("migration report = %+v, %v; want 2 sealed", report, err)
	}
	for _, k := range []string{"llm.api_key", "clone.dle_token"} {
		if !crypto.IsSealed(rawConfigValue(t, pool, k, 0)) {
			t.Fatalf("%s still plaintext after startup migration", k)
		}
	}
	if got, _ := overrideValue(t, sealed, "llm.api_key"); got != fixtureAPIKey {
		t.Fatalf("migrated key reads %q", got)
	}
	report, err = sealed.SealStoredSecrets(ctx)
	if err != nil || report.Sealed != 0 || report.Rotated != 0 {
		t.Fatalf("second run = %+v, %v; want no work", report, err)
	}
}

func TestConfigSecrets_RotationReadsOldAndReSealsUnderNew(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	ctx := context.Background()
	oldKR, newKR := keyringOf(t, 1), keyringOf(t, 2, 1)
	_ = NewConfigStore(pool).WithKeyring(oldKR).SetOverride(ctx, "llm.api_key",
		fixtureAPIKey, 0, 1)

	rotated := NewConfigStore(pool).WithKeyring(newKR)
	if got, _ := overrideValue(t, rotated, "llm.api_key"); got != fixtureAPIKey {
		t.Fatalf("read under previous key = %q", got)
	}
	report, err := rotated.SealStoredSecrets(ctx)
	if err != nil || report.Rotated != 1 {
		t.Fatalf("rotation report = %+v, %v; want 1 rotated", report, err)
	}
	kid, _ := crypto.SealedKeyID(rawConfigValue(t, pool, "llm.api_key", 0))
	if kid != newKR.ActiveKeyID() {
		t.Fatalf("after rotation kid = %q, want %q", kid, newKR.ActiveKeyID())
	}
	newOnly := NewConfigStore(pool).WithKeyring(keyringOf(t, 2))
	if got, _ := overrideValue(t, newOnly, "llm.api_key"); got != fixtureAPIKey {
		t.Fatalf("after rotation the old key is no longer needed; read %q", got)
	}
}

func TestConfigSecrets_WriteUnderRotationUsesActiveKey(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	kr := keyringOf(t, 2, 1)
	cs := NewConfigStore(pool).WithKeyring(kr)
	_ = cs.SetOverride(context.Background(), "llm.api_key", fixtureAPIKey, 0, 1)
	kid, _ := crypto.SealedKeyID(rawConfigValue(t, pool, "llm.api_key", 0))
	if kid != kr.ActiveKeyID() {
		t.Fatalf("write sealed under %q, want active %q", kid, kr.ActiveKeyID())
	}
}

func TestConfigSecrets_UnreadableSecretIsOmittedNotFatal(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	ctx := context.Background()
	_ = NewConfigStore(pool).WithKeyring(keyringOf(t, 1)).SetOverride(ctx,
		"llm.api_key", fixtureAPIKey, 0, 1)
	_ = NewConfigStore(pool).WithKeyring(nil).SetOverride(ctx,
		"llm.timeout_seconds", "33", 0, 1)

	for name, kr := range map[string]*crypto.Keyring{
		"wrong key": keyringOf(t, 9), "no key": nil,
	} {
		t.Run(name, func(t *testing.T) {
			cs := NewConfigStore(pool).WithKeyring(kr)
			if v, ok := overrideValue(t, cs, "llm.api_key"); ok {
				t.Fatalf("undecryptable secret returned as %q", v)
			}
			if v, ok := overrideValue(t, cs, "llm.timeout_seconds"); !ok || v != "33" {
				t.Fatalf("other overrides lost: (%q, %v)", v, ok)
			}
			report, err := cs.SealStoredSecrets(ctx)
			if err != nil || report.Unreadable != 1 {
				t.Fatalf("report = %+v, %v; want 1 unreadable", report, err)
			}
			if !crypto.IsSealed(rawConfigValue(t, pool, "llm.api_key", 0)) {
				t.Fatal("an unreadable row must be left untouched")
			}
		})
	}
}

func TestConfigSecrets_CiphertextBoundToKeyAndScope(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	ctx := context.Background()
	kr := keyringOf(t, 1)
	cs := NewConfigStore(pool).WithKeyring(kr)
	_ = cs.SetOverride(ctx, "llm.api_key", fixtureAPIKey, 0, 1)
	sealed := rawConfigValue(t, pool, "llm.api_key", 0)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value) VALUES
		('clone.dle_token', $1)`, sealed); err != nil {
		t.Fatal(err)
	}
	if v, ok := overrideValue(t, cs, "clone.dle_token"); ok {
		t.Fatalf("ciphertext moved to another key decrypted to %q", v)
	}
}

func TestConfigSecrets_AuditNeverHoldsSecretOrCiphertext(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	ctx := context.Background()
	cs := NewConfigStore(pool).WithKeyring(keyringOf(t, 1))
	_ = cs.SetOverride(ctx, "llm.api_key", fixtureAPIKey, 0, 1)
	_ = cs.SetOverride(ctx, "llm.api_key", fixtureAPIKey+"-2", 0, 1)
	var leaks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE key = 'llm.api_key' AND (COALESCE(old_value, '') LIKE '%sk-fixture%'
		   OR new_value LIKE '%sk-fixture%' OR new_value LIKE 'sage-enc:%'
		   OR COALESCE(old_value, '') LIKE 'sage-enc:%')`).Scan(&leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Fatalf("%d audit rows hold a secret or its ciphertext", leaks)
	}
}

func TestConfigSecrets_MergedConfigMasksDecryptedSecret(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	ctx := context.Background()
	cs := NewConfigStore(pool).WithKeyring(keyringOf(t, 1))
	_ = cs.SetOverride(ctx, "llm.api_key", fixtureAPIKey, 0, 1)
	merged, err := cs.GetMergedConfig(ctx, config.DefaultConfig(), 0)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := merged["llm.api_key"].(map[string]any)
	shown, _ := entry["value"].(string)
	if strings.Contains(shown, fixtureAPIKey) || strings.HasPrefix(shown, "sage-enc") {
		t.Fatalf("merged config exposes %q", shown)
	}
	if !strings.HasSuffix(shown, fixtureAPIKey[len(fixtureAPIKey)-4:]) {
		t.Fatalf("merged config value %q is not the masked plaintext", shown)
	}
}

func TestConfigSecrets_ConcurrentStartupMigration(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	ctx := context.Background()
	_ = NewConfigStore(pool).WithKeyring(nil).SetOverride(ctx, "llm.api_key",
		fixtureAPIKey, 0, 1)
	kr := keyringOf(t, 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := NewConfigStore(pool).WithKeyring(kr).SealStoredSecrets(ctx)
			if err != nil {
				t.Errorf("SealStoredSecrets: %v", err)
				return
			}
			mu.Lock()
			total += r.Sealed
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("row sealed %d times across concurrent sidecars, want exactly 1", total)
	}
	if got, _ := overrideValue(t, NewConfigStore(pool).WithKeyring(kr), "llm.api_key"); got !=
		fixtureAPIKey {
		t.Fatalf("after concurrent migration the key reads %q", got)
	}
}

func TestConfigSecrets_DefaultKeyringIsUsedByNewConfigStore(t *testing.T) {
	pool := setupConfigTestDB(t)
	cleanupSecretRows(t, pool)
	kr := keyringOf(t, 5)
	SetDefaultConfigKeyring(kr)
	t.Cleanup(func() { SetDefaultConfigKeyring(nil) })
	if err := NewConfigStore(pool).SetOverride(context.Background(), "llm.api_key",
		fixtureAPIKey, 0, 1); err != nil {
		t.Fatal(err)
	}
	kid, _ := crypto.SealedKeyID(rawConfigValue(t, pool, "llm.api_key", 0))
	if kid != kr.ActiveKeyID() {
		t.Fatalf("NewConfigStore ignored the process keyring: kid %q", kid)
	}
}
