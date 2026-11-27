package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
)

// controlKeySet is the key material that seals secrets stored in one
// control database: the active and previous keys derived with that
// database's KDF salt, and the keyring built from them.
type controlKeySet struct {
	active, previous []byte
	kr               *crypto.Keyring
}

type controlKeyCacheKey struct {
	pool             *pgxpool.Pool
	active, previous string
}

var (
	secretsMu sync.Mutex
	// startupKeys is resolveEncryptionKey's decision; the zero value
	// (tests, tools) behaves like a configured key.
	startupKeys    keyResolution
	controlKeySets = map[controlKeyCacheKey]*controlKeySet{}
	// keyMissing records that a database's generated key was lost.
	keyMissing bool
)

// resolveEncryptionKeyOrExit applies resolveEncryptionKey to the startup
// config; a refused key file stops the process.
func resolveEncryptionKeyOrExit() {
	res, err := resolveEncryptionKey(cfg, logWarn)
	if err != nil {
		logError("startup", "encryption key: %v", err)
		os.Exit(1)
	}
	setStartupKeys(res)
}

func setStartupKeys(res keyResolution) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	startupKeys = res
}

// resetSecretsAtRest forgets every resolved key (tests).
func resetSecretsAtRest() {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	startupKeys, keyMissing = keyResolution{}, false
	controlKeySets = map[controlKeyCacheKey]*controlKeySet{}
}

// applyGeneratedKey gives a reload candidate the generated key.
func applyGeneratedKey(candidate *config.Config) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	startupKeys.applyTo(candidate)
}

// controlKeys returns the keys for secrets stored in pool under the
// startup encryption_key, or nil when secrets there stay plaintext.
func controlKeys(ctx context.Context, pool *pgxpool.Pool) (*controlKeySet, error) {
	passphrase := ""
	if cfg != nil {
		passphrase = cfg.EncryptionKey
	}
	return controlKeysFor(ctx, pool, passphrase)
}

// controlKeysFor derives (once per pool and passphrase) the keys for pool.
// With no passphrase it settles a pending generated key; a lost generated
// key logs an ERROR naming the file and key id and yields nil keys.
func controlKeysFor(
	ctx context.Context, pool *pgxpool.Pool, passphrase string,
) (*controlKeySet, error) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	previous := ""
	if cfg != nil {
		previous = cfg.EncryptionKeyPrevious
	}
	cacheKey := controlKeyCacheKey{pool: pool, active: passphrase, previous: previous}
	if set, ok := controlKeySets[cacheKey]; ok {
		return set, nil
	}
	set, err := loadControlKeys(ctx, pool, passphrase, previous)
	if errors.Is(err, errGeneratedKeyMissing) {
		keyMissing = true
		logError("security", "%v", err)
		set, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	controlKeySets[controlKeyCacheKey{pool: pool, active: activePassphrase(passphrase),
		previous: previous}] = set
	controlKeySets[cacheKey] = set
	return set, nil
}

// activePassphrase is passphrase, or the generated key settle produced.
func activePassphrase(passphrase string) string {
	if passphrase == "" && cfg != nil {
		return cfg.EncryptionKey
	}
	return passphrase
}

func loadControlKeys(
	ctx context.Context, pool *pgxpool.Pool, passphrase, previous string,
) (*controlKeySet, error) {
	recorded, err := schema.ReadGeneratedKeyID(ctx, pool)
	if err != nil {
		return nil, err
	}
	if passphrase == "" && cfg != nil {
		if err := startupKeys.settle(recorded, cfg); err != nil {
			return nil, err
		}
		passphrase = cfg.EncryptionKey
	}
	if passphrase == "" {
		return buildControlKeys("", previous, nil)
	}
	salt, err := schema.ReadOrCreateKDFSalt(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("reading KDF salt: %w", err)
	}
	set, err := buildControlKeys(passphrase, previous, salt)
	if err != nil {
		return nil, err
	}
	return set, recordGeneratedKey(ctx, pool, recorded, set)
}

func recordGeneratedKey(
	ctx context.Context, pool *pgxpool.Pool, recorded string, set *controlKeySet,
) error {
	record, err := checkGeneratedKeyID(startupKeys, recorded, set.kr.ActiveKeyID())
	if err != nil || !record {
		return err
	}
	stored, err := schema.RecordGeneratedKeyID(ctx, pool, set.kr.ActiveKeyID())
	if err != nil {
		return err
	}
	_, err = checkGeneratedKeyID(startupKeys, stored, set.kr.ActiveKeyID())
	return err
}

// deriveControlKeys derives the active and previous keys with argon2id
// and the database's salt.
func deriveControlKeys(active, previous string, salt []byte) (*controlKeySet, error) {
	if len(salt) < 8 {
		return nil, fmt.Errorf("config secrets: KDF salt too short (%d bytes)", len(salt))
	}
	set := &controlKeySet{active: crypto.DeriveKey(active, salt)}
	if previous != "" {
		set.previous = crypto.DeriveKey(previous, salt)
	}
	kr, err := crypto.NewKeyring(set.active, set.previous)
	if err != nil {
		return nil, fmt.Errorf("config secrets: %w", err)
	}
	set.kr = kr
	return set, nil
}

// secretsPosture reports how secrets are stored in pool for
// GET /api/v1/security/posture. It never reports key material.
func secretsPosture(ctx context.Context, pool *pgxpool.Pool) api.SecretsPosture {
	secretsMu.Lock()
	source, missing := startupKeys.Source, keyMissing
	secretsMu.Unlock()
	posture := api.SecretsPosture{SecretsAtRest: "sealed", KeySource: string(source)}
	if source == "" || source == keySourcePending {
		posture.KeySource = string(keySourceNone)
		if cfg != nil && cfg.EncryptionKey != "" {
			posture.KeySource = string(keySourceConfigured)
		}
	}
	keys, err := notificationKeys(ctx, pool)
	switch {
	case missing:
		posture.SecretsAtRest = "key_missing"
	case err != nil || keys == nil:
		posture.SecretsAtRest = "plaintext"
	}
	if pool != nil {
		n, err := store.CountPlaintextSecrets(ctx, pool)
		if err != nil {
			logWarn("security", "counting plaintext secrets: %v", err)
		}
		posture.PlaintextRows = n
	}
	return posture
}

// secretsPostureSource serves the posture of secrets stored in pool.
func secretsPostureSource(pool *pgxpool.Pool) func(context.Context) (api.SecretsPosture, error) {
	return func(ctx context.Context) (api.SecretsPosture, error) {
		return secretsPosture(ctx, pool), nil
	}
}
