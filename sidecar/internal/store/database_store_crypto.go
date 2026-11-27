package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// decryptPassword opens a stored password. A sage-enc:v1 value opens with
// the keyring under the row's AAD. Older AAD-less values open with the
// current key, then the legacy keys derived at construction, then a
// previous key; a legacy-key hit is re-encrypted in place so the fallback
// runs once per row (ResealDatabasePasswords seals the rest at startup).
// It derives no key. A value no key opens is ErrWrongEncryptionKey naming
// the database, logged once per database id per process.
func (s *DatabaseStore) decryptPassword(
	ctx context.Context, id int, enc []byte,
) (string, error) {
	if crypto.IsSealed(string(enc)) {
		password, _, err := s.keyring.Open(string(enc), dbPasswordAAD(id))
		if errors.Is(err, crypto.ErrOpenFailed) || errors.Is(err, crypto.ErrUnknownKeyID) {
			s.logWrongKeyOnce(id)
			return "", fmt.Errorf("database %d: %w: %w", id, crypto.ErrWrongEncryptionKey, err)
		}
		if err != nil {
			return "", fmt.Errorf("decrypting password for database %d: %w", id, err)
		}
		return password, nil
	}
	password, resealed, err := s.openLegacy(enc)
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		s.logWrongKeyOnce(id)
		return "", fmt.Errorf("database %d: %w", id, crypto.ErrWrongEncryptionKey)
	}
	if err != nil {
		return "", fmt.Errorf("decrypting password for database %d: %w", id, err)
	}
	if resealed != nil {
		updated, err := s.replaceCiphertext(ctx, id, enc, resealed)
		if err != nil {
			return "", err
		}
		if updated {
			log.Printf("store: database %d: migrated legacy password encryption", id)
		}
	}
	return password, nil
}

// openLegacy opens an AAD-less password: the current key and the pre-v0.9
// keys first (resealed is set for a pre-v0.9 hit), then a previous key.
func (s *DatabaseStore) openLegacy(enc []byte) (string, []byte, error) {
	password, resealed, err := crypto.DecryptLegacy(enc, s.encryptKey, s.legacy)
	if !errors.Is(err, crypto.ErrWrongEncryptionKey) || s.keyring == nil {
		return password, resealed, err
	}
	password, _, unboundErr := s.keyring.OpenUnbound(enc)
	if unboundErr != nil {
		return "", nil, err
	}
	return password, nil, nil
}

func (s *DatabaseStore) logWrongKeyOnce(id int) {
	if _, seen := s.wrongKeyLogged.LoadOrStore(id, struct{}{}); seen {
		return
	}
	slog.Error("the stored password does not open with encryption_key; "+
		"was encryption_key changed? Re-enter it or restore the old key",
		"database_id", id)
}

// replaceCiphertext swaps the legacy ciphertext only if it is unchanged, so
// a concurrent update of the password is never overwritten. It reports
// whether this call wrote the row; a concurrent reader that lost the race
// gets false.
func (s *DatabaseStore) replaceCiphertext(
	ctx context.Context, id int, old, next []byte,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.databases
		    SET password_enc = $1, updated_at = now()
		  WHERE id = $2 AND password_enc = $3`,
		next, id, old,
	)
	if err != nil {
		return false, fmt.Errorf("re-encrypting legacy password for database %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}
