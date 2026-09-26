package store

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// decryptPassword decrypts a stored password with the current key and, when
// key migration is configured, falls back to the legacy derivations. A
// legacy hit is re-encrypted in place so the fallback runs once per row.
func (s *DatabaseStore) decryptPassword(
	ctx context.Context, id int, enc []byte,
) (string, error) {
	password, err := crypto.Decrypt(enc, s.encryptKey)
	if err == nil {
		return password, nil
	}
	if s.passphrase == "" {
		return "", fmt.Errorf("decrypting password: %w", err)
	}
	password, reEncrypted, needsReEncrypt, migErr :=
		crypto.DecryptWithMigration(enc, s.passphrase, s.salt)
	if migErr != nil {
		return "", fmt.Errorf("decrypting password: %w", migErr)
	}
	if needsReEncrypt {
		if err := s.replaceCiphertext(ctx, id, enc, reEncrypted); err != nil {
			return "", err
		}
		log.Printf("store: database %d: migrated legacy password encryption", id)
	}
	return password, nil
}

// replaceCiphertext swaps the legacy ciphertext only if it is unchanged, so
// a concurrent update of the password is never overwritten.
func (s *DatabaseStore) replaceCiphertext(
	ctx context.Context, id int, old, next []byte,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.databases
		    SET password_enc = $1, updated_at = now()
		  WHERE id = $2 AND password_enc = $3`,
		next, id, old,
	)
	if err != nil {
		return fmt.Errorf("re-encrypting legacy password for database %d: %w", id, err)
	}
	return nil
}
