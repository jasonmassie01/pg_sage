//go:build integration

package store

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// security-09b integration: the action credential of a meta-DB member on
// a real sage.databases.

func actionURL(t *testing.T, s *DatabaseStore, id int) *url.URL {
	t.Helper()
	dsn, err := s.GetActionConnectionString(context.Background(), id)
	if err != nil {
		t.Fatalf("GetActionConnectionString(%d): %v", id, err)
	}
	if dsn == "" {
		return nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse action DSN: %v", err)
	}
	return u
}

func assertActionUser(t *testing.T, s *DatabaseStore, id int, user, password string) {
	t.Helper()
	u := actionURL(t, s, id)
	if user == "" {
		if u != nil {
			t.Fatalf("action DSN for user %q, want none", u.User.Username())
		}
		return
	}
	if u == nil {
		t.Fatalf("no action DSN, want role %q", user)
	}
	pw, _ := u.User.Password()
	if u.User.Username() != user || pw != password {
		t.Fatalf("action DSN user %q password %q, want %q %q",
			u.User.Username(), pw, user, password)
	}
	rec, err := s.Get(context.Background(), id)
	if err != nil || rec.ActionUsername != user {
		t.Fatalf("record ActionUsername = %v, %v; want %q", rec, err, user)
	}
}

// CHECK-15: no stored action password is anything but sage-enc:v1.
func unsealedActionPasswords(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.databases
		WHERE action_password_enc IS NOT NULL
		  AND left(convert_from(action_password_enc,'UTF8'),11) <> 'sage-enc:v1'`,
	).Scan(&n); err != nil {
		t.Fatalf("CHECK-15 query: %v", err)
	}
	return n
}

func TestActionCredential_CreateSealsAndTargetsTheMembersDatabase(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	id := mustCreate(t, s, withAction("actor", "act-pw"))
	assertActionUser(t, s, id, "actor", "act-pw")
	u := actionURL(t, s, id)
	in := validInput()
	if u.Hostname() != in.Host || u.Port() != "5432" ||
		strings.TrimPrefix(u.Path, "/") != in.DatabaseName ||
		u.Query().Get("sslmode") != in.SSLMode {
		t.Fatalf("action DSN %s does not target the member's host, port, database "+
			"and sslmode", u.Redacted())
	}
	obs, err := s.GetConnectionString(ctx, id)
	if err != nil || connectionPassword(t, obs) != in.Password {
		t.Fatalf("observation DSN changed: %v", err)
	}
	if err := config.ValidateActionDSN(obs, u.String()); err != nil {
		t.Fatalf("stored action DSN fails ValidateActionDSN: %v", err)
	}
	var enc []byte
	if err := pool.QueryRow(ctx, `SELECT action_password_enc FROM sage.databases
		WHERE id = $1`, id).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "act-pw") ||
		storedKeyID(t, enc) != crypto.KeyIDFor(rotationKeyA) {
		t.Fatalf("stored action password %.20q is not sealed under key A", enc)
	}
	if n := unsealedActionPasswords(t, pool); n != 0 {
		t.Fatalf("CHECK-15: %d unsealed action passwords", n)
	}
}

// Nil/empty: a member without the credential reads "" and nil.
func TestActionCredential_NoneIsSingleIdentity(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	id := mustCreate(t, s, validInput())
	dsn, err := s.GetActionConnectionString(ctx, id)
	if dsn != "" || err != nil {
		t.Fatalf("GetActionConnectionString = %q, %v; want empty", dsn, err)
	}
	rec, _ := s.Get(ctx, id)
	if rec.ActionUsername != "" {
		t.Fatalf("ActionUsername = %q, want empty", rec.ActionUsername)
	}
	if _, err := s.GetActionConnectionString(ctx, id+1000); err == nil {
		t.Fatal("an unknown database id read an action DSN without error")
	}
}

// State transitions: add, change the password, rename keeping the
// password, leave unchanged (nil), remove.
func TestActionCredential_UpdateTransitions(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	id := mustCreate(t, s, validInput())
	steps := []struct {
		name           string
		action         *ActionCredentialInput
		wantUser, want string
	}{
		{"add", &ActionCredentialInput{Username: "actor", Password: "p1"}, "actor", "p1"},
		{"change password", &ActionCredentialInput{Username: "actor", Password: "p2"},
			"actor", "p2"},
		{"rename keeps password", &ActionCredentialInput{Username: "actor2"}, "actor2", "p2"},
		{"nil leaves it", nil, "actor2", "p2"},
		{"remove", &ActionCredentialInput{}, "", ""},
	}
	for _, step := range steps {
		in := validInput()
		in.Action = step.action
		prospective, err := s.GetUpdateActionConnectionString(ctx, id, in)
		if err != nil {
			t.Fatalf("%s: GetUpdateActionConnectionString: %v", step.name, err)
		}
		if err := s.Update(ctx, id, in); err != nil {
			t.Fatalf("%s: Update: %v", step.name, err)
		}
		assertActionUser(t, s, id, step.wantUser, step.want)
		stored, _ := s.GetActionConnectionString(ctx, id)
		if prospective != stored {
			t.Fatalf("%s: prospective DSN differs from the stored one", step.name)
		}
	}
	var user, enc any
	if err := pool.QueryRow(ctx, `SELECT action_username, action_password_enc
		FROM sage.databases WHERE id = $1`, id).Scan(&user, &enc); err != nil {
		t.Fatal(err)
	}
	if user != nil || enc != nil {
		t.Fatalf("removed credential left %v / %v", user, enc)
	}
}

// Invalid input: a rename with no stored password, and an observation
// username changed onto the action role, are refused by the store.
func TestActionCredential_UpdateRefusals(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	id := mustCreate(t, s, validInput())
	in := withAction("actor", "")
	if err := s.Update(ctx, id, in); !errors.Is(err, ErrActionIdentity) ||
		!errors.Is(err, ErrValidation) {
		t.Fatalf("rename with no stored password: %v, want ErrActionIdentity", err)
	}
	if _, err := s.GetUpdateActionConnectionString(ctx, id, in); !errors.Is(err,
		ErrActionIdentity) {
		t.Fatalf("prospective rename with no stored password: %v", err)
	}
	if err := s.Update(ctx, id, withAction("actor", "pw")); err != nil {
		t.Fatal(err)
	}
	moved := validInput()
	moved.Username = "actor"
	err := s.Update(ctx, id, moved)
	if !errors.Is(err, ErrActionIdentity) || !errors.Is(err, config.ErrActionDSNTarget) {
		t.Fatalf("observation username onto the action role: %v", err)
	}
	if n := unsealedActionPasswords(t, pool); n != 0 {
		t.Fatalf("CHECK-15: %d unsealed action passwords", n)
	}
	assertActionUser(t, s, id, "actor", "pw")
}

// Boundary: a sealed action password copied onto the observation column of
// its row, or onto another row's action column, does not open.
func TestActionCredential_CopiedCiphertextDoesNotOpen(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	captureSlog(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	a := mustCreate(t, s, withAction("actor", "act-a"))
	other := validInput()
	other.Name = "other-db"
	b := mustCreate(t, s, other)
	if _, err := pool.Exec(ctx, `UPDATE sage.databases SET password_enc = action_password_enc
		WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConnectionString(ctx, a); !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("action ciphertext as the observation password: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.databases d SET action_username = 'actor',
		action_password_enc = (SELECT action_password_enc FROM sage.databases WHERE id = $1)
		WHERE d.id = $2`, a, b); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetActionConnectionString(ctx, b)
	if !errors.Is(err, crypto.ErrWrongEncryptionKey) ||
		!strings.Contains(err.Error(), "database "+strconv.Itoa(b)) {
		t.Fatalf("copied to another row: %v, want ErrWrongEncryptionKey naming %d", err, b)
	}
}

// Rotation: sealed under key A, a store on key B with A as previous
// reseals the action password once; afterwards key B alone opens it, and a
// key that opens nothing reports it unreadable.
func TestActionCredential_ResealRotatesActionPasswords(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	captureSlog(t)
	id := mustCreate(t, NewDatabaseStore(pool, rotationKeyA), withAction("actor", "act-pw"))
	mustCreate(t, NewDatabaseStore(pool, rotationKeyA), func() DatabaseInput {
		in := validInput()
		in.Name = "no-action"
		return in
	}())
	rotated := NewDatabaseStore(pool, rotationKeyB).WithPreviousKey(rotationKeyA)
	report, err := rotated.ResealActionPasswords(ctx)
	if err != nil || report.Rotated != 1 || report.Sealed != 0 || report.Unreadable != 0 {
		t.Fatalf("first reseal = %+v, %v; want 1 rotated", report, err)
	}
	again, err := rotated.ResealActionPasswords(ctx)
	if err != nil || again != (SecretSealReport{}) {
		t.Fatalf("second reseal = %+v, %v; want nothing", again, err)
	}
	assertActionUser(t, NewDatabaseStore(pool, rotationKeyB), id, "actor", "act-pw")
	keyC := crypto.DeriveKey("rotation-key-c", []byte("rotation-salt-16"))
	lost, err := NewDatabaseStore(pool, keyC).ResealActionPasswords(ctx)
	if err != nil || lost.Unreadable != 1 || lost.Rotated != 0 {
		t.Fatalf("reseal under an unrelated key = %+v, %v; want 1 unreadable", lost, err)
	}
	if n := unsealedActionPasswords(t, pool); n != 0 {
		t.Fatalf("CHECK-15: %d unsealed action passwords", n)
	}
}

// Concurrency: parallel updates of one member's credential always leave a
// username and password written by the same update.
func TestActionCredential_ConcurrentUpdatesStayPaired(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	id := mustCreate(t, s, validInput())
	pairs := map[string]string{"actor1": "pw-1", "actor2": "pw-2"}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		for user, pw := range pairs {
			wg.Add(1)
			go func(user, pw string) {
				defer wg.Done()
				in := validInput()
				in.Action = &ActionCredentialInput{Username: user, Password: pw}
				errs <- s.Update(ctx, id, in)
			}(user, pw)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Update: %v", err)
		}
	}
	u := actionURL(t, s, id)
	pw, _ := u.User.Password()
	if pairs[u.User.Username()] != pw {
		t.Fatalf("final credential %q/%q mixes two updates", u.User.Username(), pw)
	}
}

// Re-entering the action password is the documented remedy for a stored one
// no key opens: a prospective update that names both the role and a new
// password needs nothing stored, while one that keeps the stored password
// still fails closed with ErrWrongEncryptionKey.
func TestActionCredential_ReenterAfterLostKey(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	captureSlog(t)
	id := mustCreate(t, NewDatabaseStore(pool, rotationKeyA), withAction("actor", "old-pw"))
	keyC := crypto.DeriveKey("rotation-key-c", []byte("rotation-salt-16"))
	lost := NewDatabaseStore(pool, keyC)
	keep := withAction("actor", "")
	if _, err := lost.GetUpdateActionConnectionString(ctx, id, keep); !errors.Is(err,
		crypto.ErrWrongEncryptionKey) {
		t.Fatalf("keeping an unreadable password: %v, want ErrWrongEncryptionKey", err)
	}
	reentered := withAction("actor2", "new-pw")
	dsn, err := lost.GetUpdateActionConnectionString(ctx, id, reentered)
	if err != nil {
		t.Fatalf("re-entered credential refused: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse prospective DSN: %v", err)
	}
	if pw, _ := u.User.Password(); u.User.Username() != "actor2" || pw != "new-pw" {
		t.Fatalf("prospective DSN role %q password %q, want actor2 new-pw",
			u.User.Username(), pw)
	}
	if err := lost.Update(ctx, id, reentered); err != nil {
		t.Fatalf("Update with the re-entered credential: %v", err)
	}
	assertActionUser(t, lost, id, "actor2", "new-pw")
}
