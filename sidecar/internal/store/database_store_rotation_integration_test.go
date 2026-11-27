//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/testdb"
)

var (
	rotationKeyA = crypto.DeriveKey("rotation-key-a", []byte("rotation-salt-16"))
	rotationKeyB = crypto.DeriveKey("rotation-key-b", []byte("rotation-salt-16"))
)

// liveDatabaseInput describes the test server itself, so a stored record
// can really connect.
func liveDatabaseInput(t *testing.T, name string) DatabaseInput {
	t.Helper()
	u, err := url.Parse(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 5432
	}
	pw, _ := u.User.Password()
	return DatabaseInput{
		Name: name, Host: u.Hostname(), Port: port,
		DatabaseName: strings.TrimPrefix(u.Path, "/"), Username: u.User.Username(),
		Password: pw, SSLMode: "disable", MaxConnections: 2,
		TrustLevel: "observation", ExecutionMode: "approval",
	}
}

func mustCreate(t *testing.T, s *DatabaseStore, in DatabaseInput) int {
	t.Helper()
	id, err := s.Create(context.Background(), in, 1)
	if err != nil {
		t.Fatalf("Create %s: %v", in.Name, err)
	}
	return id
}

func storedKeyID(t *testing.T, enc []byte) string {
	t.Helper()
	kid, err := crypto.SealedKeyID(string(enc))
	if err != nil {
		t.Fatalf("stored value %.20q is not sealed: %v", enc, err)
	}
	return kid
}

func TestDatabasePassword_CreateSealsUnderRowAAD(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	in := validInput()
	in.Password = "row-bound-pw"
	id := mustCreate(t, s, in)
	enc := storedCiphertext(t, pool, id)
	if kid := storedKeyID(t, enc); kid != crypto.KeyIDFor(rotationKeyA) {
		t.Fatalf("key id = %s, want key A", kid)
	}
	if strings.Contains(string(enc), "row-bound-pw") {
		t.Fatal("stored value holds the password")
	}
	if _, _, err := s.keyring.Open(string(enc), dbPasswordAAD(id)); err != nil {
		t.Fatalf("stored value does not open under its own row id %d: %v", id, err)
	}
	conn, err := s.GetConnectionString(ctx, id)
	if err != nil || connectionPassword(t, conn) != "row-bound-pw" {
		t.Fatalf("GetConnectionString = %q, %v", conn, err)
	}
	in.Password = "updated-pw"
	if err := s.Update(ctx, id, in); err != nil {
		t.Fatalf("Update: %v", err)
	}
	conn, err = s.GetConnectionString(ctx, id)
	if err != nil || connectionPassword(t, conn) != "updated-pw" {
		t.Fatalf("after Update GetConnectionString = %q, %v", conn, err)
	}
}

// Nil/empty: a new database still needs a password (validation, unchanged
// by sealing); an update with an empty password keeps the sealed one.
func TestDatabasePassword_EmptyPasswordRules(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	in := validInput()
	in.Password = ""
	if _, err := s.Create(ctx, in, 1); !errors.Is(err, ErrValidation) {
		t.Fatalf("Create with empty password: err = %v, want ErrValidation", err)
	}
	in.Password = "kept-pw"
	id := mustCreate(t, s, in)
	before := storedCiphertext(t, pool, id)
	in.Password = ""
	if err := s.Update(ctx, id, in); err != nil {
		t.Fatalf("Update with empty password: %v", err)
	}
	if after := storedCiphertext(t, pool, id); string(after) != string(before) {
		t.Fatal("an update with an empty password rewrote the sealed password")
	}
	conn, err := s.GetConnectionString(ctx, id)
	if err != nil || connectionPassword(t, conn) != "kept-pw" {
		t.Fatalf("GetConnectionString = %q, %v; want kept-pw", conn, err)
	}
}

func TestDatabasePassword_CopiedCiphertextFailsToOpen(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	captureSlog(t)
	s := NewDatabaseStore(pool, rotationKeyA)
	in := validInput()
	in.Name, in.Password = "copy-src", "src-pw"
	src := mustCreate(t, s, in)
	in.Name, in.Password = "copy-dst", "dst-pw"
	dst := mustCreate(t, s, in)
	if _, err := pool.Exec(ctx, `UPDATE sage.databases SET password_enc =
		(SELECT password_enc FROM sage.databases WHERE id = $1) WHERE id = $2`,
		src, dst); err != nil {
		t.Fatalf("copy password_enc: %v", err)
	}
	conn, err := s.GetConnectionString(ctx, dst)
	if !errors.Is(err, crypto.ErrOpenFailed) {
		t.Fatalf("copied password: conn %q err %v, want ErrOpenFailed", conn, err)
	}
	report, err := s.ResealDatabasePasswords(ctx)
	if err != nil || report.Unreadable != 1 {
		t.Fatalf("reseal report = %+v, %v; want 1 unreadable", report, err)
	}
}

// State transitions: raw crypto.Encrypt -> sage-enc:v1 under A -> rotated
// to B. The record that describes the test server really connects after.
func TestDatabasePassword_RotationReachesActiveKey(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	storeA := NewDatabaseStore(pool, rotationKeyA)
	sealedID := mustCreate(t, storeA, liveDatabaseInput(t, "rot-sealed-a"))
	rawID := mustCreate(t, storeA, liveDatabaseInput(t, "rot-raw-a"))
	raw, err := crypto.Encrypt(liveDatabaseInput(t, "x").Password, rotationKeyA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.databases SET password_enc = $1
		WHERE id = $2`, raw, rawID); err != nil {
		t.Fatalf("install raw ciphertext: %v", err)
	}

	storeB := NewDatabaseStore(pool, rotationKeyB).WithPreviousKey(rotationKeyA)
	report, err := storeB.ResealDatabasePasswords(ctx)
	if err != nil {
		t.Fatalf("ResealDatabasePasswords: %v", err)
	}
	if report.Sealed != 1 || report.Rotated != 1 || report.Unreadable != 0 {
		t.Fatalf("report = %+v, want sealed 1 rotated 1", report)
	}
	onlyB := NewDatabaseStore(pool, rotationKeyB)
	for _, id := range []int{sealedID, rawID} {
		kid := storedKeyID(t, storedCiphertext(t, pool, id))
		if kid != crypto.KeyIDFor(rotationKeyB) {
			t.Errorf("database %d key id = %s, want key B", id, kid)
		}
		connectWith(t, onlyB, id)
	}
	again, err := storeB.ResealDatabasePasswords(ctx)
	if err != nil || again != (SecretSealReport{}) {
		t.Fatalf("second reseal = %+v, %v; want nothing to do", again, err)
	}
}

func connectWith(t *testing.T, s *DatabaseStore, id int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connStr, err := s.GetConnectionString(ctx, id)
	if err != nil {
		t.Fatalf("database %d: GetConnectionString: %v", id, err)
	}
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("database %d does not connect: %v", id, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("database %d: SELECT 1 = %d, %v", id, one, err)
	}
}

// Concurrency: a password written between the reseal's read and its swap
// survives, because the compare-and-swap loses.
func TestDatabasePassword_ResealLosesToConcurrentUpdate(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	storeA := NewDatabaseStore(pool, rotationKeyA)
	in := validInput()
	in.Password = "old-pw"
	id := mustCreate(t, storeA, in)
	storeB := NewDatabaseStore(pool, rotationKeyB).WithPreviousKey(rotationKeyA)
	rows, err := storeB.passwordRows(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("passwordRows = %v, %v", rows, err)
	}
	in.Password = "new-pw"
	if err := storeB.Update(ctx, id, in); err != nil {
		t.Fatalf("Update: %v", err)
	}
	report, err := storeB.resealRows(ctx, rows)
	if err != nil || report.Rotated != 0 {
		t.Fatalf("reseal after update = %+v, %v; want the swap to lose", report, err)
	}
	conn, err := storeB.GetConnectionString(ctx, id)
	if err != nil || connectionPassword(t, conn) != "new-pw" {
		t.Fatalf("password after race = %q, %v; want new-pw", conn, err)
	}
}

func TestDatabasePassword_ResealRacingUpdatesNeverLosesAWrite(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	storeA := NewDatabaseStore(pool, rotationKeyA)
	in := validInput()
	in.Password = "pw-0"
	id := mustCreate(t, storeA, in)
	storeB := NewDatabaseStore(pool, rotationKeyB).WithPreviousKey(rotationKeyA)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 1; i <= 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := storeB.ResealDatabasePasswords(ctx)
			errs <- err
		}()
		go func(i int) {
			defer wg.Done()
			upd := in
			upd.Password = fmt.Sprintf("pw-%d", i)
			errs <- storeB.Update(ctx, id, upd)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent op: %v", err)
		}
	}
	conn, err := storeB.GetConnectionString(ctx, id)
	if pw := connectionPassword(t, conn); err != nil || !strings.HasPrefix(pw, "pw-") ||
		pw == "pw-0" {
		t.Fatalf("final password = %q, %v", conn, err)
	}
	if kid := storedKeyID(t, storedCiphertext(t, pool, id)); kid != crypto.KeyIDFor(rotationKeyB) {
		t.Fatalf("final key id = %s, want key B", kid)
	}
}
