package schema

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// security-08: generated_key_id names the key pg_sage generated into
// <data_dir>/encryption.key for this database, so a lost or replaced key
// file is detected at startup instead of silently replaced.

const cryptoMetaKeyIDTimeout = 10 * time.Second

const cryptoMetaKeyIDSQL = `/* pg_sage crypto_meta_key_id v1 */
ALTER TABLE sage.crypto_meta ADD COLUMN IF NOT EXISTS generated_key_id text`

// migrateCryptoMetaKeyID adds the nullable generated_key_id column under a
// short lock_timeout. It is idempotent. When the catalog already shows the
// column it runs no DDL, so a restart takes no ACCESS EXCLUSIVE lock on
// sage.crypto_meta (the config-04 precheck).
func migrateCryptoMetaKeyID(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, cryptoMetaKeyIDTimeout)
	defer cancel()
	stmts := classifyStep(cryptoMetaKeyIDSQL)
	if err := markDone(qctx, db, stmts); err != nil {
		return fmt.Errorf("crypto_meta key id migration: precheck: %w", err)
	}
	if !pendingWork(stmts) {
		return nil
	}
	tx, err := db.Begin(qctx)
	if err != nil {
		return fmt.Errorf("crypto_meta key id migration: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	if _, err := tx.Exec(qctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return fmt.Errorf("crypto_meta key id migration: lock_timeout: %w", err)
	}
	if _, err := tx.Exec(qctx, cryptoMetaKeyIDSQL); err != nil {
		return fmt.Errorf("crypto_meta key id migration: adding generated_key_id: %w", err)
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("crypto_meta key id migration: commit: %w", err)
	}
	return nil
}

// ReadGeneratedKeyID returns the recorded generated key id, or "" when no
// key was ever generated for this database (or no crypto_meta row exists).
func ReadGeneratedKeyID(ctx context.Context, db bootstrapDB) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id *string
	err := db.QueryRow(qctx, `/* pg_sage */ SELECT generated_key_id
		FROM sage.crypto_meta WHERE id = 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("crypto_meta: reading generated_key_id: %w", err)
	}
	if id == nil {
		return "", nil
	}
	return *id, nil
}

// RecordGeneratedKeyID records keyID unless an id is already recorded, and
// returns the id that is recorded afterwards (the first writer's). The KDF
// salt row must exist (ReadOrCreateKDFSalt).
func RecordGeneratedKeyID(ctx context.Context, db bootstrapDB, keyID string) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var stored *string
	err := db.QueryRow(qctx, `/* pg_sage */ UPDATE sage.crypto_meta
		SET generated_key_id = COALESCE(generated_key_id, $1)
		WHERE id = 1 RETURNING generated_key_id`, keyID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("crypto_meta: no KDF salt row to record the generated key id on")
	}
	if err != nil {
		return "", fmt.Errorf("crypto_meta: recording generated_key_id: %w", err)
	}
	return *stored, nil
}
