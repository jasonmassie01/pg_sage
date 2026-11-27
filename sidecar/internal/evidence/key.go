package evidence

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// Where a signing key came from, as GET /api/v1/audit/evidence/key says.
const (
	// KeySourceEnv is a key the operator supplied (signing_key_env).
	KeySourceEnv = "env"
	// KeySourceGenerated is the key the sidecar generated and stores in
	// sage.evidence_signing_key, sealed under the encryption key.
	KeySourceGenerated = "generated"
)

// ErrStoredKeyUnreadable means sage.evidence_signing_key holds a key this
// encryption key cannot open, or one that does not match its key id. The
// row is left alone: replacing it would orphan every pack it signed.
var ErrStoredKeyUnreadable = errors.New("evidence: the stored signing key cannot be read")

// signingKeyAAD binds the sealed key to its row.
const signingKeyAAD = "sage.evidence_signing_key:1"

// KeyStore is the database that holds the stored key: the control
// database (the meta database in fleet mode).
type KeyStore interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// StoredKey is the key LoadOrCreateKey read or created. Created is true
// for the one caller whose insert made the row. ResealErr is set when the
// key opened only under the previous encryption key and moving it to the
// active one failed; the key is still usable.
type StoredKey struct {
	Key       ed25519.PrivateKey
	KeyID     string
	Created   bool
	ResealErr error
}

// LoadOrCreateKey returns the stored evidence-signing key, creating it on
// first use. kr is the encryption keyring of the database's secrets; nil
// (no encryption key) returns crypto.ErrNoKeyring and creates nothing. Two
// callers at once agree: the insert is ON CONFLICT DO NOTHING and both
// read the row back. A row that does not open returns
// ErrStoredKeyUnreadable and is never overwritten.
func LoadOrCreateKey(ctx context.Context, db KeyStore, kr *crypto.Keyring) (StoredKey,
	error) {
	if kr == nil {
		return StoredKey{}, fmt.Errorf("evidence signing key: %w", crypto.ErrNoKeyring)
	}
	if db == nil {
		return StoredKey{}, errors.New("evidence signing key: no control database")
	}
	keyID, sealed, found, err := readStoredKey(ctx, db)
	if err != nil {
		return StoredKey{}, err
	}
	created := false
	if !found {
		if created, err = insertStoredKey(ctx, db, kr); err != nil {
			return StoredKey{}, err
		}
		if keyID, sealed, _, err = readStoredKey(ctx, db); err != nil {
			return StoredKey{}, err
		}
	}
	key, sealedBy, err := openStoredKey(kr, keyID, sealed)
	if err != nil {
		return StoredKey{}, err
	}
	out := StoredKey{Key: key, KeyID: keyID, Created: created}
	if sealedBy != kr.ActiveKeyID() {
		out.ResealErr = resealStoredKey(ctx, db, kr, key, sealed)
	}
	return out, nil
}

func readStoredKey(ctx context.Context, db KeyStore) (string, string, bool, error) {
	var keyID, sealed string
	err := db.QueryRow(ctx, `SELECT key_id, sealed FROM sage.evidence_signing_key
		WHERE id = 1`).Scan(&keyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("evidence signing key: read "+
			"sage.evidence_signing_key: %w", err)
	}
	return keyID, sealed, true, nil
}

// insertStoredKey generates a key and inserts it unless a row exists; it
// reports whether this insert made the row.
func insertStoredKey(ctx context.Context, db KeyStore, kr *crypto.Keyring) (bool, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return false, fmt.Errorf("evidence signing key: generate: %w", err)
	}
	sealed, err := sealKey(kr, priv)
	if err != nil {
		return false, err
	}
	tag, err := db.Exec(ctx, `INSERT INTO sage.evidence_signing_key (id, key_id, sealed)
		VALUES (1, $1, $2) ON CONFLICT (id) DO NOTHING`, KeyID(pub), sealed)
	if err != nil {
		return false, fmt.Errorf("evidence signing key: insert into "+
			"sage.evidence_signing_key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func sealKey(kr *crypto.Keyring, key ed25519.PrivateKey) (string, error) {
	pemText, err := MarshalSigningKey(key)
	if err != nil {
		return "", err
	}
	sealed, err := kr.Seal(pemText, signingKeyAAD)
	if err != nil {
		return "", fmt.Errorf("evidence signing key: seal: %w", err)
	}
	return sealed, nil
}

// openStoredKey opens the sealed key and checks it against its key id; it
// returns the id of the encryption key that sealed it.
func openStoredKey(kr *crypto.Keyring, keyID, sealed string) (ed25519.PrivateKey, string,
	error) {
	pemText, sealedBy, err := kr.Open(sealed, signingKeyAAD)
	if err != nil {
		return nil, "", fmt.Errorf("%w (key id %s): %w", ErrStoredKeyUnreadable, keyID, err)
	}
	key, err := LoadSigningKey(pemText)
	if err != nil {
		return nil, "", fmt.Errorf("%w (key id %s): %w", ErrStoredKeyUnreadable, keyID, err)
	}
	if got := KeyID(key.Public().(ed25519.PublicKey)); got != keyID {
		return nil, "", fmt.Errorf("%w: the row names key id %s but holds key %s",
			ErrStoredKeyUnreadable, keyID, got)
	}
	return key, sealedBy, nil
}

// resealStoredKey moves the key to the active encryption key, so dropping
// the previous one after a rotation keeps it. It only replaces the value it
// opened; a concurrent re-seal by another sidecar wins harmlessly.
func resealStoredKey(ctx context.Context, db KeyStore, kr *crypto.Keyring,
	key ed25519.PrivateKey, old string) error {
	sealed, err := sealKey(kr, key)
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `UPDATE sage.evidence_signing_key SET sealed = $1
		WHERE id = 1 AND sealed = $2`, sealed, old); err != nil {
		return fmt.Errorf("evidence signing key: re-seal under the active encryption "+
			"key: %w", err)
	}
	return nil
}
