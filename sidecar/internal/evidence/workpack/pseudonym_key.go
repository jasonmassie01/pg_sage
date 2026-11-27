package workpack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// The pseudonym key (evid-02c): 32 random bytes per install that key the
// model tier's identifier HMAC. crypto.Keyring only seals and opens, and a
// key derived from the active encryption key would change on rotation, so
// it is created on first use and stored in sage.workload_pseudonym_key
// sealed under the keyring, the way evidence.LoadOrCreateKey stores the
// signing key. A row opened only by a previous encryption key is re-sealed
// under the active one.

// ErrPseudonymKeyUnreadable means the stored key does not open under this
// keyring. The row is left alone: replacing it would change every
// pseudonym.
var ErrPseudonymKeyUnreadable = errors.New("workpack: the stored pseudonym key cannot " +
	"be read")

const pseudonymKeyAAD = "sage.workload_pseudonym_key:1"

// LoadOrCreatePseudonymKey returns the install's pseudonym key, creating
// it on first use. kr nil returns crypto.ErrNoKeyring and creates nothing.
// Two callers at once agree: the insert is ON CONFLICT DO NOTHING and both
// read the row back.
func LoadOrCreatePseudonymKey(ctx context.Context, db evidence.KeyStore,
	kr *crypto.Keyring) ([]byte, error) {
	if kr == nil {
		return nil, fmt.Errorf("workload pseudonym key: %w", crypto.ErrNoKeyring)
	}
	if db == nil {
		return nil, errors.New("workload pseudonym key: no control database")
	}
	sealed, found, err := readPseudonymKey(ctx, db)
	if err != nil {
		return nil, err
	}
	if !found {
		if err := insertPseudonymKey(ctx, db, kr); err != nil {
			return nil, err
		}
		if sealed, _, err = readPseudonymKey(ctx, db); err != nil {
			return nil, err
		}
	}
	plain, sealedBy, err := kr.Open(sealed, pseudonymKeyAAD)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPseudonymKeyUnreadable, err)
	}
	key, err := hex.DecodeString(plain)
	if err != nil || len(key) != PseudonymKeyBytes {
		return nil, fmt.Errorf("%w: it is not %d hex-encoded bytes",
			ErrPseudonymKeyUnreadable, PseudonymKeyBytes)
	}
	if sealedBy != kr.ActiveKeyID() {
		if err := resealPseudonymKey(ctx, db, kr, key, sealed); err != nil {
			return nil, err
		}
	}
	return key, nil
}

func readPseudonymKey(ctx context.Context, db evidence.KeyStore) (string, bool, error) {
	var sealed string
	err := db.QueryRow(ctx, `SELECT sealed FROM sage.workload_pseudonym_key
		WHERE id = 1`).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("workload pseudonym key: read "+
			"sage.workload_pseudonym_key: %w", err)
	}
	return sealed, true, nil
}

func insertPseudonymKey(ctx context.Context, db evidence.KeyStore, kr *crypto.Keyring) error {
	key := make([]byte, PseudonymKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("workload pseudonym key: generate: %w", err)
	}
	sealed, err := kr.Seal(hex.EncodeToString(key), pseudonymKeyAAD)
	if err != nil {
		return fmt.Errorf("workload pseudonym key: seal: %w", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO sage.workload_pseudonym_key (id, sealed)
		VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, sealed); err != nil {
		return fmt.Errorf("workload pseudonym key: insert into "+
			"sage.workload_pseudonym_key: %w", err)
	}
	return nil
}

// resealPseudonymKey moves the key to the active encryption key; it only
// replaces the value it opened, so a concurrent re-seal wins harmlessly.
func resealPseudonymKey(ctx context.Context, db evidence.KeyStore, kr *crypto.Keyring,
	key []byte, old string) error {
	sealed, err := kr.Seal(hex.EncodeToString(key), pseudonymKeyAAD)
	if err != nil {
		return fmt.Errorf("workload pseudonym key: re-seal: %w", err)
	}
	if _, err := db.Exec(ctx, `UPDATE sage.workload_pseudonym_key SET sealed = $1
		WHERE id = 1 AND sealed = $2`, sealed, old); err != nil {
		return fmt.Errorf("workload pseudonym key: re-seal under the active "+
			"encryption key: %w", err)
	}
	return nil
}
