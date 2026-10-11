package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// CG-01 / E1: secret config keys set through the API are sealed at rest
// with AES-256-GCM under encryption_key, and each value names its key id.
// Without a key the values stay plaintext, as before, and pg_sage warns.

// ErrSealedValueRejected refuses a client-supplied secret that already
// looks sealed: only pg_sage writes sealed values.
var ErrSealedValueRejected = errors.New(
	"validate: secret value must not start with the sealed-value prefix")

var (
	defaultConfigKeyring atomic.Pointer[crypto.Keyring]
	plaintextWriteWarn   sync.Once
)

// SetDefaultConfigKeyring sets the keyring every NewConfigStore uses; nil
// stores secrets in plaintext. Startup calls it once the key is derived.
func SetDefaultConfigKeyring(kr *crypto.Keyring) {
	defaultConfigKeyring.Store(kr)
}

// WithKeyring replaces this store's keyring (tests, tools).
func (s *ConfigStore) WithKeyring(kr *crypto.Keyring) *ConfigStore {
	s.keyring = kr
	return s
}

// secretAAD binds a sealed value to its key and scope, so a ciphertext
// copied to another key or database does not decrypt.
func secretAAD(key string, databaseID int) string {
	return fmt.Sprintf("sage.config|%s|%d", key, databaseID)
}

// sealForStorage returns the value to persist for key.
func (s *ConfigStore) sealForStorage(key, value string, databaseID int) (string, error) {
	if !isSecretConfigKey(key) || value == "" {
		return value, nil
	}
	if crypto.IsSealed(value) {
		return "", ErrSealedValueRejected
	}
	if s.keyring == nil {
		plaintextWriteWarn.Do(func() {
			slog.Warn("config secret stored in plaintext: set encryption_key " +
				"(SAGE_ENCRYPTION_KEY or SAGE_ENCRYPTION_KEY_FILE) to encrypt it")
		})
		return value, nil
	}
	return s.keyring.Seal(value, secretAAD(key, databaseID))
}

// openSecrets decrypts sealed secret overrides in place. A value that
// cannot be opened (no key, unknown key id, tampering) is dropped with an
// error log naming the key and key id, so it never applies as ciphertext.
func (s *ConfigStore) openSecrets(overrides []ConfigOverride) []ConfigOverride {
	out := overrides[:0]
	for _, o := range overrides {
		if isSecretConfigKey(o.Key) && crypto.IsSealed(o.Value) {
			plain, _, err := s.keyring.Open(o.Value, secretAAD(o.Key, o.DatabaseID))
			if err != nil {
				kid, _ := crypto.SealedKeyID(o.Value)
				slog.Error("config secret cannot be decrypted; ignoring the override",
					"key", o.Key, "database_id", o.DatabaseID, "key_id", kid,
					"hint", "set encryption_key, or encryption_key_previous during a rotation",
					"error", err)
				continue
			}
			o.Value = plain
		}
		out = append(out, o)
	}
	return out
}

// SecretSealReport counts what SealStoredSecrets found.
type SecretSealReport struct {
	Sealed     int // plaintext rows sealed now
	Rotated    int // rows re-sealed from a previous key to the active key
	Plaintext  int // plaintext rows left (no keyring)
	Unreadable int // sealed rows no configured key can open
}

type storedSecret struct {
	key        string
	databaseID int
	value      string
}

// SealStoredSecrets seals plaintext secret rows and re-seals rows sealed
// under a previous key. Each update is a compare-and-swap on the old value,
// so concurrent sidecars and concurrent API writes never lose a value.
// Without a keyring it only counts plaintext rows.
func (s *ConfigStore) SealStoredSecrets(ctx context.Context) (SecretSealReport, error) {
	var report SecretSealReport
	rows, err := s.storedSecrets(ctx)
	if err != nil {
		return report, err
	}
	for _, row := range rows {
		next, kind := s.resealed(row)
		switch kind {
		case "plaintext":
			report.Plaintext++
		case "unreadable":
			report.Unreadable++
		case "sealed", "rotated":
			swapped, err := s.swapSecret(ctx, row, next)
			if err != nil {
				return report, err
			}
			if swapped && kind == "sealed" {
				report.Sealed++
			} else if swapped {
				report.Rotated++
			}
		}
	}
	return report, nil
}

// resealed decides what to do with one stored secret.
func (s *ConfigStore) resealed(row storedSecret) (string, string) {
	aad := secretAAD(row.key, row.databaseID)
	if !crypto.IsSealed(row.value) {
		if s.keyring == nil {
			return "", "plaintext"
		}
		next, err := s.keyring.Seal(row.value, aad)
		if err != nil {
			return "", "unreadable"
		}
		return next, "sealed"
	}
	plain, kid, err := s.keyring.Open(row.value, aad)
	if err != nil {
		return "", "unreadable"
	}
	if kid == s.keyring.ActiveKeyID() {
		return "", "current"
	}
	next, err := s.keyring.Seal(plain, aad)
	if err != nil {
		return "", "unreadable"
	}
	return next, "rotated"
}

func (s *ConfigStore) storedSecrets(ctx context.Context) ([]storedSecret, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx,
		`/* pg_sage config_secrets v1 */ SELECT key, COALESCE(database_id, 0), value
		 FROM sage.config WHERE key = ANY($1) AND value <> ''`,
		secretConfigKeys())
	if err != nil {
		return nil, fmt.Errorf("listing stored config secrets: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedSecret, error) {
		var ss storedSecret
		err := r.Scan(&ss.key, &ss.databaseID, &ss.value)
		return ss, err
	})
}

func (s *ConfigStore) swapSecret(
	ctx context.Context, row storedSecret, next string,
) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx,
		`/* pg_sage config_secrets v1 */ UPDATE sage.config SET value = $1
		 WHERE key = $2 AND COALESCE(database_id, 0) = $3 AND value = $4`,
		next, row.key, row.databaseID, row.value)
	if err != nil {
		return false, fmt.Errorf("sealing config secret %s: %w", row.key, err)
	}
	return tag.RowsAffected() == 1, nil
}
