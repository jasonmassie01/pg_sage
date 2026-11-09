package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// security-08: database passwords are sealed with sage-enc:v1 under the
// row's AAD. These tests use a nil pool: none of these paths may write.
// No concurrent-access tests here: the CAS race is an integration test
// (database_store_rotation_integration_test.go).

func mustSealPassword(t *testing.T, s *DatabaseStore, id int, pw string) []byte {
	t.Helper()
	sealedPW, err := s.keyring.Seal(pw, dbPasswordAAD(id))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return []byte(sealedPW)
}

func TestDBPasswordAAD_NamesTableRowAndColumn(t *testing.T) {
	if got := dbPasswordAAD(42); got != "sage.databases|42|password" {
		t.Fatalf("dbPasswordAAD(42) = %q", got)
	}
}

func TestDecryptPassword_SealedOpensForItsRow(t *testing.T) {
	s := NewDatabaseStore(nil, unitKeyA)
	enc := mustSealPassword(t, s, 3, "pw-3")
	pw, err := s.decryptPassword(context.Background(), 3, enc)
	if err != nil || pw != "pw-3" {
		t.Fatalf("decryptPassword = %q, %v", pw, err)
	}
}

func TestDecryptPassword_SealedCopiedToAnotherRowFails(t *testing.T) {
	captureSlog(t)
	s := NewDatabaseStore(nil, unitKeyA)
	enc := mustSealPassword(t, s, 1, "pw-1")
	pw, err := s.decryptPassword(context.Background(), 2, enc)
	if !errors.Is(err, crypto.ErrOpenFailed) {
		t.Fatalf("err = %v, want ErrOpenFailed", err)
	}
	if !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Errorf("err = %v, want it to also wrap ErrWrongEncryptionKey", err)
	}
	if pw != "" || !strings.Contains(err.Error(), "database 2") {
		t.Errorf("pw = %q, err = %q; want empty and naming database 2", pw, err)
	}
}

func TestDecryptPassword_SealedUnknownKeyNamesDatabase(t *testing.T) {
	logs := captureSlog(t)
	sealer := NewDatabaseStore(nil, unitKeyA)
	enc := mustSealPassword(t, sealer, 5, "pw-5")
	s := NewDatabaseStore(nil, unitKeyB)
	_, err := s.decryptPassword(context.Background(), 5, enc)
	if !errors.Is(err, crypto.ErrUnknownKeyID) || !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("err = %v, want ErrUnknownKeyID and ErrWrongEncryptionKey", err)
	}
	if !strings.Contains(logs.String(), "database_id=5") {
		t.Errorf("logs = %q, want the database id", logs.String())
	}
	if strings.Contains(logs.String(), "pw-5") {
		t.Error("log holds the password")
	}
}

func TestDecryptPassword_SealedUnderPreviousKeyOpens(t *testing.T) {
	sealer := NewDatabaseStore(nil, unitKeyA)
	enc := mustSealPassword(t, sealer, 9, "pw-9")
	s := NewDatabaseStore(nil, unitKeyB).WithPreviousKey(unitKeyA)
	pw, err := s.decryptPassword(context.Background(), 9, enc)
	if err != nil || pw != "pw-9" {
		t.Fatalf("decryptPassword = %q, %v", pw, err)
	}
}

func TestDecryptPassword_RawUnderPreviousKeyOpensWithoutWrite(t *testing.T) {
	s := NewDatabaseStore(nil, unitKeyB).WithPreviousKey(unitKeyA)
	pw, err := s.decryptPassword(context.Background(), 4, sealed(t, "raw-a", unitKeyA))
	if err != nil || pw != "raw-a" {
		t.Fatalf("decryptPassword = %q, %v", pw, err)
	}
}

func TestDecryptPassword_MalformedSealedIsNotWrongKey(t *testing.T) {
	s := NewDatabaseStore(nil, unitKeyA)
	_, err := s.decryptPassword(context.Background(), 1, []byte(crypto.SealedPrefix+"x"))
	if !errors.Is(err, crypto.ErrMalformedSealed) {
		t.Fatalf("err = %v, want ErrMalformedSealed", err)
	}
	if errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Error("a malformed value is not a wrong key")
	}
}

func TestResealPassword_Decisions(t *testing.T) {
	captureSlog(t)
	sealerA := NewDatabaseStore(nil, unitKeyA)
	s := NewDatabaseStore(nil, unitKeyB).WithPreviousKey(unitKeyA).
		WithKeyMigration("passphrase-b")
	other := crypto.DeriveKey("passphrase-c", unitSalt)
	cases := []struct {
		name string
		enc  []byte
		want string
	}{
		{"sealed under active", mustSealPassword(t, s, 1, "p"), resealCurrent},
		{"sealed under previous", mustSealPassword(t, sealerA, 1, "p"), resealRotated},
		{"raw under active", sealed(t, "p", unitKeyB), resealSealed},
		{"raw under previous", sealed(t, "p", unitKeyA), resealSealed},
		{"raw under legacy v1", sealed(t, "p", crypto.DeriveKeyV1("passphrase-b")), resealSealed},
		{"raw under unknown key", sealed(t, "p", other), resealUnreadable},
		{"sealed for another row", mustSealPassword(t, s, 2, "p"), resealUnreadable},
		{"truncated raw", []byte("short"), resealUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, kind := s.resealPassword(1, tc.enc)
			if kind != tc.want {
				t.Fatalf("kind = %q, want %q", kind, tc.want)
			}
			if kind != resealSealed && kind != resealRotated {
				if next != nil {
					t.Errorf("next = %q for %s, want nil", next, kind)
				}
				return
			}
			kid, err := crypto.SealedKeyID(string(next))
			if err != nil || kid != s.keyring.ActiveKeyID() {
				t.Fatalf("next key id = %q, %v; want active %s", kid, err,
					s.keyring.ActiveKeyID())
			}
			pw, _, err := s.keyring.Open(string(next), dbPasswordAAD(1))
			if err != nil || pw != "p" {
				t.Fatalf("resealed opens to %q, %v", pw, err)
			}
		})
	}
}

func TestCreate_NoKeyRefusedWithActionableError(t *testing.T) {
	for _, key := range [][]byte{nil, {}, []byte("short")} {
		s := NewDatabaseStore(nil, key)
		_, err := s.Create(context.Background(), validInput(), 1)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("key %d bytes: err = %v, want ErrValidation", len(key), err)
		}
		for _, want := range []string{"encryption_key", "data_dir"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %s", err, want)
			}
		}
	}
}

func TestWithPreviousKey_IgnoresEmptyAndKeepsActive(t *testing.T) {
	s := NewDatabaseStore(nil, unitKeyA)
	active := s.keyring.ActiveKeyID()
	s.WithPreviousKey(nil)
	if s.keyring.ActiveKeyID() != active {
		t.Fatal("an empty previous key changed the active key")
	}
	s.WithPreviousKey(unitKeyB)
	if s.keyring.ActiveKeyID() != active {
		t.Fatal("a previous key changed the active key")
	}
	if NewDatabaseStore(nil, nil).WithPreviousKey(unitKeyB).keyring != nil {
		t.Fatal("a previous key without an active key built a keyring")
	}
}
