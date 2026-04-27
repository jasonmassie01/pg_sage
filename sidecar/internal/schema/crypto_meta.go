package schema

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// ReadOrCreateKDFSalt returns the per-deployment argon2id salt from
// sage.crypto_meta, generating and persisting a new random salt on
// first call. The salt is stable across restarts so that keys derived
// at startup can decrypt records encrypted in prior runs.
//
// Bootstrap must have been run (creating the sage.crypto_meta table)
// before this is called.
func ReadOrCreateKDFSalt(
	ctx context.Context, pool *pgxpool.Pool,
) ([]byte, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var salt []byte
	err := pool.QueryRow(
		qctx, "SELECT kdf_salt FROM sage.crypto_meta WHERE id = 1",
	).Scan(&salt)
	if err == nil {
		if len(salt) < 8 {
			return nil, fmt.Errorf(
				"crypto_meta: persisted kdf_salt is too short "+
					"(%d bytes)", len(salt),
			)
		}
		return salt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"crypto_meta: reading kdf_salt: %w", err,
		)
	}

	// First run for this meta DB — generate and persist.
	newSalt, err := crypto.NewKDFSalt()
	if err != nil {
		return nil, err
	}

	ictx, icancel := context.WithTimeout(ctx, 5*time.Second)
	defer icancel()

	// ON CONFLICT covers the race where two sidecars race to insert;
	// the first wins and we return whatever landed in the row.
	_, err = pool.Exec(ictx,
		`INSERT INTO sage.crypto_meta (id, kdf_salt)
		 VALUES (1, $1)
		 ON CONFLICT (id) DO NOTHING`,
		newSalt,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"crypto_meta: inserting kdf_salt: %w", err,
		)
	}

	// Re-read to pick up the winning row (could be ours or a peer's).
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	err = pool.QueryRow(
		rctx, "SELECT kdf_salt FROM sage.crypto_meta WHERE id = 1",
	).Scan(&salt)
	if err != nil {
		return nil, fmt.Errorf(
			"crypto_meta: re-reading kdf_salt after insert: %w", err,
		)
	}
	return salt, nil
}
