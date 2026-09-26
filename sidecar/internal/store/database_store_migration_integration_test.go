//go:build integration

package store

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
)

const migrationPassphrase = "legacy-upgrade-passphrase"

var migrationSalt = []byte("upgrade-salt-016")

// seedLegacyPassword creates a record, then replaces its ciphertext with one
// produced by a pre-v0.9 key derivation, exactly as a v0.8.x meta DB holds it.
func seedLegacyPassword(
	t *testing.T, pool *pgxpool.Pool, name string, legacyKey []byte,
) int {
	t.Helper()
	ctx := context.Background()
	seedStore := NewDatabaseStore(pool, testKey)
	input := validInput()
	input.Name = name
	id, err := seedStore.Create(ctx, input, 1)
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	legacy, err := crypto.Encrypt(input.Password, legacyKey)
	if err != nil {
		t.Fatalf("legacy encrypt: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sage.databases SET password_enc = $1 WHERE id = $2`,
		legacy, id); err != nil {
		t.Fatalf("install legacy ciphertext: %v", err)
	}
	return id
}

func storedCiphertext(t *testing.T, pool *pgxpool.Pool, id int) []byte {
	t.Helper()
	var enc []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT password_enc FROM sage.databases WHERE id = $1`, id,
	).Scan(&enc); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	return enc
}

func connectionPassword(t *testing.T, connStr string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	password, _ := u.User.Password()
	return password
}

// G5-B05: credentials written by v0.8.4/v0.8.5 must survive the upgrade.
func TestGetConnectionString_MigratesLegacyCiphertext(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	currentKey := crypto.DeriveKey(migrationPassphrase, migrationSalt)
	legacyKeys := map[string][]byte{
		"legacy-v1": crypto.DeriveKeyV1(migrationPassphrase),
		"legacy-v2": crypto.DeriveKeyV2Legacy(migrationPassphrase),
	}
	for name, legacyKey := range legacyKeys {
		t.Run(name, func(t *testing.T) {
			id := seedLegacyPassword(t, pool, name, legacyKey)
			s := NewDatabaseStore(pool, currentKey).
				WithKeyMigration(migrationPassphrase, migrationSalt)

			connStr, err := s.GetConnectionString(ctx, id)

			if err != nil {
				t.Fatalf("GetConnectionString: %v", err)
			}
			if got := connectionPassword(t, connStr); got != validInput().Password {
				t.Fatalf("password = %q, want %q", got, validInput().Password)
			}
			reencrypted, err := crypto.Decrypt(storedCiphertext(t, pool, id), currentKey)
			if err != nil {
				t.Fatalf("stored ciphertext was not re-encrypted: %v", err)
			}
			if reencrypted != validInput().Password {
				t.Fatalf("re-encrypted password = %q", reencrypted)
			}
		})
	}
}

func TestGetUpdateConnectionString_MigratesLegacyCiphertext(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	currentKey := crypto.DeriveKey(migrationPassphrase, migrationSalt)
	id := seedLegacyPassword(t, pool, "legacy-update",
		crypto.DeriveKeyV2Legacy(migrationPassphrase))
	s := NewDatabaseStore(pool, currentKey).
		WithKeyMigration(migrationPassphrase, migrationSalt)
	input := validInput()
	input.Name = "legacy-update"
	input.Password = ""

	connStr, err := s.GetUpdateConnectionString(ctx, id, input)

	if err != nil {
		t.Fatalf("GetUpdateConnectionString: %v", err)
	}
	if got := connectionPassword(t, connStr); got != validInput().Password {
		t.Fatalf("password = %q, want %q", got, validInput().Password)
	}
	if _, err := crypto.Decrypt(storedCiphertext(t, pool, id), currentKey); err != nil {
		t.Fatalf("stored ciphertext was not re-encrypted: %v", err)
	}
}

func TestGetConnectionString_WrongPassphraseStillFails(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	id := seedLegacyPassword(t, pool, "legacy-wrong",
		crypto.DeriveKeyV1("a-different-passphrase"))
	before := storedCiphertext(t, pool, id)
	s := NewDatabaseStore(pool, crypto.DeriveKey(migrationPassphrase, migrationSalt)).
		WithKeyMigration(migrationPassphrase, migrationSalt)

	if _, err := s.GetConnectionString(ctx, id); err == nil {
		t.Fatal("wrong passphrase decrypted a credential")
	}
	if string(storedCiphertext(t, pool, id)) != string(before) {
		t.Fatal("failed decryption must not rewrite the stored ciphertext")
	}
}
