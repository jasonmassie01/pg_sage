package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/store"
)

// initConfigSecrets resolves the control database's keys (CG-01,
// security-08) from encryption_key, encryption_key_previous or a generated
// key, makes the keyring the default for every ConfigStore, and seals or
// rotates the config and channel secrets already stored. It runs before
// persisted overrides are read.
func initConfigSecrets(ctx context.Context, controlPool *pgxpool.Pool) error {
	keys, err := controlKeys(ctx, controlPool)
	if err != nil {
		return fmt.Errorf("config secrets: %w", err)
	}
	var kr *crypto.Keyring
	channels := store.NewNotificationStore(controlPool, nil)
	if keys != nil {
		kr = keys.kr
		channels.WithSecretKey(keys.active).WithPreviousSecretKey(keys.previous)
	}
	store.SetDefaultConfigKeyring(kr)
	report, err := store.NewConfigStore(controlPool).SealStoredSecrets(ctx)
	if err != nil {
		return fmt.Errorf("sealing stored config secrets: %w", err)
	}
	reportConfigSecretSeal(kr != nil, report, logStructured)
	channelReport, err := channels.ResealChannelSecrets(ctx)
	if err != nil {
		return fmt.Errorf("sealing stored channel secrets: %w", err)
	}
	reportResealCounts("notification channel secrets", channelReport, logStructured)
	return nil
}

// resealDatabasePasswords moves every stored database password to
// sage-enc:v1 under the active key (security-08), and every action password
// to the active key (security-09b).
func resealDatabasePasswords(ctx context.Context, s *store.DatabaseStore) error {
	return resealStoredPasswords(ctx, s, logStructured)
}

// resealStoredPasswords reseals both password columns and reports each
// column's counts separately.
func resealStoredPasswords(
	ctx context.Context, s *store.DatabaseStore,
	logf func(level, component, msg string, args ...any),
) error {
	report, err := s.ResealDatabasePasswords(ctx)
	if err != nil {
		return fmt.Errorf("sealing stored database passwords: %w", err)
	}
	reportResealCounts("database passwords", report, logf)
	actions, err := s.ResealActionPasswords(ctx)
	if err != nil {
		return fmt.Errorf("re-sealing stored action passwords: %w", err)
	}
	reportResealCounts("action passwords", actions, logf)
	return nil
}

// metaDatabaseStore opens sage.databases with the meta keys and moves every
// stored password to sage-enc:v1 under the active key (security-08).
func metaDatabaseStore(
	ctx context.Context, metaPool *pgxpool.Pool, keys *controlKeySet, passphrase string,
) (*store.DatabaseStore, error) {
	var active, previous []byte
	if keys != nil {
		active, previous = keys.active, keys.previous
	}
	dbStore := store.NewDatabaseStore(metaPool, active).WithPreviousKey(previous)
	if passphrase != "" {
		// Credentials written by v0.8.4/v0.8.5 used legacy key
		// derivations; decrypt and re-encrypt them on first read (G5-B05).
		dbStore.WithKeyMigration(passphrase)
	}
	if err := resealDatabasePasswords(ctx, dbStore); err != nil {
		return nil, err
	}
	return dbStore, nil
}

// reportResealCounts logs what a startup re-seal did, never values.
func reportResealCounts(
	what string, r store.SecretSealReport,
	logf func(level, component, msg string, args ...any),
) {
	if r.Unreadable > 0 {
		logf("ERROR", "security", "%d %s cannot be decrypted and are left as "+
			"stored: configure the key that sealed them as encryption_key, or as "+
			"encryption_key_previous during a rotation", r.Unreadable, what)
	}
	if r.Sealed > 0 || r.Rotated > 0 {
		logf("INFO", "security", "%s at rest: sealed %d, rotated %d",
			what, r.Sealed, r.Rotated)
	}
}

// configSecretKeyring is the keyring that seals config secrets stored in
// controlPool, or nil when they stay plaintext.
func configSecretKeyring(
	ctx context.Context, controlPool *pgxpool.Pool,
) (*crypto.Keyring, error) {
	keys, err := controlKeys(ctx, controlPool)
	if err != nil || keys == nil {
		return nil, err
	}
	return keys.kr, nil
}

// buildConfigKeyring derives the active and previous keys with argon2id
// and the per-deployment salt. No passphrase means no keyring (plaintext,
// as before E1).
func buildConfigKeyring(active, previous string, salt []byte) (*crypto.Keyring, error) {
	set, err := buildControlKeys(active, previous, salt)
	if err != nil || set == nil {
		return nil, err
	}
	return set.kr, nil
}

// buildControlKeys is buildConfigKeyring with the raw keys kept.
func buildControlKeys(active, previous string, salt []byte) (*controlKeySet, error) {
	if active == "" {
		if previous != "" {
			return nil, errors.New("encryption_key_previous is set without encryption_key")
		}
		return nil, nil
	}
	return deriveControlKeys(active, previous, salt)
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
