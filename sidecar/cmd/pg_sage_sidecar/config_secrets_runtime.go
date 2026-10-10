package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
)

// initConfigSecrets derives the config-secret keyring (CG-01) from
// encryption_key and encryption_key_previous with the deployment's KDF
// salt, makes it the default for every ConfigStore, and seals or rotates
// the secrets already stored. It runs before persisted overrides are read.
func initConfigSecrets(ctx context.Context, controlPool *pgxpool.Pool) error {
	kr, err := configSecretKeyring(ctx, controlPool)
	if err != nil {
		return err
	}
	store.SetDefaultConfigKeyring(kr)
	report, err := store.NewConfigStore(controlPool).SealStoredSecrets(ctx)
	if err != nil {
		return fmt.Errorf("sealing stored config secrets: %w", err)
	}
	reportConfigSecretSeal(kr != nil, report, logStructured)
	return nil
}

func configSecretKeyring(
	ctx context.Context, controlPool *pgxpool.Pool,
) (*crypto.Keyring, error) {
	if cfg.EncryptionKey == "" {
		return buildConfigKeyring("", cfg.EncryptionKeyPrevious, nil)
	}
	salt, err := schema.ReadOrCreateKDFSalt(ctx, controlPool)
	if err != nil {
		return nil, fmt.Errorf("config secrets: reading KDF salt: %w", err)
	}
	return buildConfigKeyring(cfg.EncryptionKey, cfg.EncryptionKeyPrevious, salt)
}

// buildConfigKeyring derives the active and previous keys with argon2id
// and the per-deployment salt. No passphrase means no keyring (plaintext,
// as before E1).
func buildConfigKeyring(active, previous string, salt []byte) (*crypto.Keyring, error) {
	if active == "" {
		if previous != "" {
			return nil, errors.New("encryption_key_previous is set without encryption_key")
		}
		return nil, nil
	}
	if len(salt) < 8 {
		return nil, fmt.Errorf("config secrets: KDF salt too short (%d bytes)", len(salt))
	}
	var prev []byte
	if previous != "" {
		prev = crypto.DeriveKey(previous, salt)
	}
	kr, err := crypto.NewKeyring(crypto.DeriveKey(active, salt), prev)
	if err != nil {
		return nil, fmt.Errorf("config secrets: %w", err)
	}
	return kr, nil
}

// reportConfigSecretSeal logs the startup outcome once: a warning when
// secrets stay plaintext, an error naming the settings when some cannot be
// decrypted, and a count of rows sealed or rotated. It never logs values.
func reportConfigSecretSeal(
	hasKeyring bool, r store.SecretSealReport,
	logf func(level, component, msg string, args ...any),
) {
	if !hasKeyring && r.Plaintext > 0 {
		logf("WARN", "config", "%d config secrets are stored in plaintext: set "+
			"encryption_key (SAGE_ENCRYPTION_KEY or SAGE_ENCRYPTION_KEY_FILE) to "+
			"encrypt them at rest", r.Plaintext)
	}
	if r.Unreadable > 0 {
		logf("ERROR", "config", "%d config secrets cannot be decrypted and are "+
			"ignored: configure the key that sealed them as encryption_key, or as "+
			"encryption_key_previous during a rotation", r.Unreadable)
	}
	if r.Sealed > 0 || r.Rotated > 0 {
		logf("INFO", "config", "config secrets at rest: sealed %d, rotated %d",
			r.Sealed, r.Rotated)
	}
}
