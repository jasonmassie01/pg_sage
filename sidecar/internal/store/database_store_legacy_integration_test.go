//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// countPasswordUpdates installs a trigger that counts writes to
// sage.databases.password_enc for the rest of the test.
func countPasswordUpdates(t *testing.T, pool *pgxpool.Pool) func() int {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS public.apiauth11_pw_updates (id int)`,
		`TRUNCATE public.apiauth11_pw_updates`,
		`CREATE OR REPLACE FUNCTION public.apiauth11_count_pw() RETURNS trigger
		 LANGUAGE plpgsql AS $$ BEGIN
		   INSERT INTO public.apiauth11_pw_updates VALUES (NEW.id); RETURN NEW;
		 END $$`,
		`DROP TRIGGER IF EXISTS apiauth11_count_pw ON sage.databases`,
		`CREATE TRIGGER apiauth11_count_pw AFTER UPDATE OF password_enc
		   ON sage.databases FOR EACH ROW EXECUTE FUNCTION public.apiauth11_count_pw()`,
	}
	for _, q := range stmts {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install update counter: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TRIGGER IF EXISTS apiauth11_count_pw ON sage.databases`)
		_, _ = pool.Exec(ctx, `DROP FUNCTION IF EXISTS public.apiauth11_count_pw()`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.apiauth11_pw_updates`)
	})
	return func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM public.apiauth11_pw_updates`).Scan(&n); err != nil {
			t.Fatalf("count updates: %v", err)
		}
		return n
	}
}

func TestGetConnectionString_LegacyResealedOnce(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	currentKey := crypto.DeriveKey(migrationPassphrase, migrationSalt)
	id := seedLegacyPassword(t, pool, "legacy-once",
		crypto.DeriveKeyV2Legacy(migrationPassphrase))
	updates := countPasswordUpdates(t, pool)
	s := NewDatabaseStore(pool, currentKey).WithKeyMigration(migrationPassphrase)

	for i := 0; i < 3; i++ {
		connStr, err := s.GetConnectionString(ctx, id)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got := connectionPassword(t, connStr); got != validInput().Password {
			t.Fatalf("read %d password = %q", i, got)
		}
	}
	if n := updates(); n != 1 {
		t.Errorf("password_enc updates = %d, want 1", n)
	}
	if pw, err := crypto.Decrypt(storedCiphertext(t, pool, id), currentKey); err != nil ||
		pw != validInput().Password {
		t.Errorf("stored value under current key = %q, %v", pw, err)
	}
}

// No -race here: the machine has no cgo. The CAS on the old value is what
// keeps the 20 readers to one write; the database enforces it.
func TestGetConnectionString_ConcurrentLegacyReadsOneCAS(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	currentKey := crypto.DeriveKey(migrationPassphrase, migrationSalt)
	id := seedLegacyPassword(t, pool, "legacy-race",
		crypto.DeriveKeyV1(migrationPassphrase))
	updates := countPasswordUpdates(t, pool)
	s := NewDatabaseStore(pool, currentKey).WithKeyMigration(migrationPassphrase)

	var wg sync.WaitGroup
	passwords := make([]string, 20)
	errs := make([]error, 20)
	for i := range passwords {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			connStr, err := s.GetConnectionString(ctx, id)
			if err != nil {
				errs[i] = err
				return
			}
			passwords[i] = connectionPassword(t, connStr)
		}(i)
	}
	wg.Wait()
	for i := range passwords {
		if errs[i] != nil || passwords[i] != validInput().Password {
			t.Errorf("reader %d = %q, %v", i, passwords[i], errs[i])
		}
	}
	if n := updates(); n != 1 {
		t.Errorf("password_enc updates = %d, want exactly one CAS", n)
	}
}

func TestGetConnectionString_WrongKeyNamesDatabase(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	id := seedLegacyPassword(t, pool, "wrong-key", testKey)
	before := storedCiphertext(t, pool, id)
	updates := countPasswordUpdates(t, pool)
	s := NewDatabaseStore(pool, crypto.DeriveKey(migrationPassphrase, migrationSalt)).
		WithKeyMigration(migrationPassphrase)

	_, err := s.GetConnectionString(ctx, id)

	if !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrWrongEncryptionKey", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("database %d", id)) {
		t.Errorf("err = %q, want it to name database %d", err, id)
	}
	if string(storedCiphertext(t, pool, id)) != string(before) || updates() != 0 {
		t.Error("a wrong-key read must not rewrite the stored ciphertext")
	}
}
