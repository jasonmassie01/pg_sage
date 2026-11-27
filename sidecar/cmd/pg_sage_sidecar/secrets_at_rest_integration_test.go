//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
)

// freshSecretsFixture gives a bootstrapped database with no crypto_meta
// row, no secret config rows, no channels and no registered databases,
// and isolates the key globals for the test.
func freshSecretsFixture(t *testing.T, dataDir string) *pgxpool.Pool {
	t.Helper()
	state, _, _ := metaLifecycleFixture(t)
	p := state.Pool
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM sage.crypto_meta`,
		`DELETE FROM sage.config WHERE key IN ('llm.api_key', 'clone.dle_token')`,
		`TRUNCATE sage.notification_channels CASCADE`,
		`DELETE FROM sage.databases`,
	} {
		if _, err := p.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	isolateKeyGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Mode, cfg.DataDir = "standalone", dataDir
	return p
}

func isolateKeyGlobals(t *testing.T) {
	t.Helper()
	oldCfg, oldMeta := cfg, globalMetaState
	resetSecretsAtRest()
	t.Cleanup(func() {
		cfg, globalMetaState = oldCfg, oldMeta
		resetSecretsAtRest()
		store.SetDefaultConfigKeyring(nil)
	})
}

func startWithDataDir(t *testing.T) {
	t.Helper()
	res, err := resolveEncryptionKey(cfg, logWarn)
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	setStartupKeys(res)
}

func TestSecretsAtRest_GeneratedKeySealsThenGuardsLoss(t *testing.T) {
	dir := t.TempDir()
	p := freshSecretsFixture(t, dir)
	ctx := context.Background()
	startWithDataDir(t)
	if err := initConfigSecrets(ctx, p); err != nil {
		t.Fatalf("initConfigSecrets: %v", err)
	}
	keyPath := dir + string(os.PathSeparator) + "encryption.key"
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("no key file generated: %v", err)
	}
	assertGeneratedKeyRecorded(t, p)
	if err := store.NewConfigStore(p).SetOverride(ctx, "llm.api_key",
		"sk-test-generated", 0, 0); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	if v := storedConfigValue(t, p, "llm.api_key"); !strings.HasPrefix(v, crypto.SealedPrefix) {
		t.Fatalf("llm.api_key stored as %.20q, want sage-enc:v1", v)
	}
	if got := secretsPosture(ctx, p); got.SecretsAtRest != "sealed" ||
		got.KeySource != "generated" || got.PlaintextRows != 0 {
		t.Fatalf("posture = %+v, want sealed/generated/0", got)
	}

	// The key file is lost: ERROR, posture key_missing, no new file.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	resetSecretsAtRest()
	cfg = config.DefaultConfig()
	cfg.Mode, cfg.DataDir = "standalone", dir
	startWithDataDir(t)
	keys, err := controlKeys(ctx, p)
	if err != nil || keys != nil {
		t.Fatalf("controlKeys after loss = %v, %v; want no keys", keys, err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a replacement key file was generated: %v", err)
	}
	if got := secretsPosture(ctx, p); got.SecretsAtRest != "key_missing" {
		t.Fatalf("posture = %+v, want key_missing", got)
	}
}

func assertGeneratedKeyRecorded(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	keys, err := controlKeys(ctx, p)
	if err != nil || keys == nil {
		t.Fatalf("controlKeys = %v, %v", keys, err)
	}
	recorded, err := schema.ReadGeneratedKeyID(ctx, p)
	if err != nil || recorded != keys.kr.ActiveKeyID() {
		t.Fatalf("recorded id = %q, %v; want %s", recorded, err, keys.kr.ActiveKeyID())
	}
}

func TestSecretsAtRest_NoDataDirStaysPlaintext(t *testing.T) {
	p := freshSecretsFixture(t, "")
	ctx := context.Background()
	startWithDataDir(t)
	if err := initConfigSecrets(ctx, p); err != nil {
		t.Fatalf("initConfigSecrets: %v", err)
	}
	if err := store.NewConfigStore(p).SetOverride(ctx, "llm.api_key",
		"sk-test-plain", 0, 0); err != nil {
		t.Fatalf("SetOverride without a key: %v", err)
	}
	if v := storedConfigValue(t, p, "llm.api_key"); v != "sk-test-plain" {
		t.Fatalf("llm.api_key stored as %q, want plaintext", v)
	}
	got := secretsPosture(ctx, p)
	if got.SecretsAtRest != "plaintext" || got.KeySource != "none" || got.PlaintextRows != 1 {
		t.Fatalf("posture = %+v, want plaintext/none/1", got)
	}
}

// State transitions: a database password and a channel secret sealed under
// key A are re-sealed under B on a restart with B active and A previous,
// and the database still connects.
func TestSecretsAtRest_RotationResealsPasswordsAndChannels(t *testing.T) {
	p := freshSecretsFixture(t, "")
	ctx := context.Background()
	dbID, chID := seedUnderKeyA(t, p)

	resetSecretsAtRest()
	cfg.EncryptionKey, cfg.EncryptionKeyPrevious = "rotation-passphrase-b", "rotation-passphrase-a"
	globalMetaState = nil
	stateB, err := initMetaDB(p, cfg.EncryptionKey)
	if err != nil {
		t.Fatalf("initMetaDB B: %v", err)
	}
	globalMetaState = stateB
	if err := initConfigSecrets(ctx, p); err != nil {
		t.Fatalf("initConfigSecrets B: %v", err)
	}
	salt, err := schema.ReadOrCreateKDFSalt(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	wantKID := crypto.KeyIDFor(crypto.DeriveKey("rotation-passphrase-b", salt))
	if kid := sealedKeyIDOf(t, p, `SELECT convert_from(password_enc, 'UTF8')
		FROM sage.databases WHERE id = $1`, dbID); kid != wantKID {
		t.Errorf("database password key id = %s, want B %s", kid, wantKID)
	}
	if kid := sealedKeyIDOf(t, p, `SELECT config->>'routing_key'
		FROM sage.notification_channels WHERE id = $1`, chID); kid != wantKID {
		t.Errorf("channel secret key id = %s, want B %s", kid, wantKID)
	}
	assertConnects(t, store.NewDatabaseStore(p, stateB.EncryptKey), dbID)
}

// seedUnderKeyA stores a database password (for the test server itself)
// and a channel secret under rotation-passphrase-a.
func seedUnderKeyA(t *testing.T, p *pgxpool.Pool) (int, int) {
	t.Helper()
	ctx := context.Background()
	cfg.EncryptionKey = "rotation-passphrase-a"
	stateA, err := initMetaDB(p, cfg.EncryptionKey)
	if err != nil {
		t.Fatalf("initMetaDB A: %v", err)
	}
	conn := p.Config().ConnConfig
	dbID, err := stateA.Store.Create(ctx, store.DatabaseInput{
		Name: "rotation-live", Host: conn.Host, Port: int(conn.Port),
		DatabaseName: conn.Database, Username: conn.User, Password: conn.Password,
		SSLMode: "disable", MaxConnections: 2, TrustLevel: "observation",
		ExecutionMode: "approval"}, 1)
	if err != nil {
		t.Fatalf("Create under A: %v", err)
	}
	chID, err := store.NewNotificationStore(p, nil).WithSecretKey(stateA.EncryptKey).
		CreateChannel(ctx, "rotation-pd", "pagerduty",
			map[string]string{"routing_key": "rk-rotation"}, 0)
	if err != nil {
		t.Fatalf("CreateChannel under A: %v", err)
	}
	return dbID, chID
}

func assertConnects(t *testing.T, s *store.DatabaseStore, id int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connStr, err := s.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("GetConnectionString under B only: %v", err)
	}
	c, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("the rotated database does not connect: %v", err)
	}
	_ = c.Close(context.Background())
}

func storedConfigValue(t *testing.T, p *pgxpool.Pool, key string) string {
	t.Helper()
	var v string
	if err := p.QueryRow(context.Background(), `SELECT value FROM sage.config
		WHERE key = $1 AND database_id IS NULL`, key).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return v
}

func sealedKeyIDOf(t *testing.T, p *pgxpool.Pool, q string, id int) string {
	t.Helper()
	var v string
	if err := p.QueryRow(context.Background(), q, id).Scan(&v); err != nil {
		t.Fatalf("read sealed value: %v", err)
	}
	kid, err := crypto.SealedKeyID(v)
	if err != nil {
		t.Fatalf("value %.20q is not sage-enc:v1: %v", v, err)
	}
	return kid
}
