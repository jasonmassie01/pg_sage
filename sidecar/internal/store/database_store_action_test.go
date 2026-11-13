package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// security-09b: a meta-DB member's action identity is a second role on the
// member's own database: only a role name and a sealed password are kept.
// These tests use a nil pool: validation and opening never reach the
// database, and a nil pool would panic if one tried to.
// No concurrent access tests here: the update race is an integration test
// (database_store_action_integration_test.go).

func TestActionPasswordAAD_NamesTableRowAndColumn(t *testing.T) {
	if got := actionPasswordAAD(42); got != "sage.databases|42|action_password" {
		t.Fatalf("actionPasswordAAD(42) = %q", got)
	}
	if actionPasswordAAD(42) == dbPasswordAAD(42) {
		t.Fatal("the action and observation passwords share an AAD")
	}
}

func withAction(user, password string) DatabaseInput {
	in := validInput()
	in.Action = &ActionCredentialInput{Username: user, Password: password}
	return in
}

type actionInputCase struct {
	name     string
	in       DatabaseInput
	create   bool
	wantErr  bool
	wantText string
	target   bool // also config.ErrActionDSNTarget
}

var actionInputCases = []actionInputCase{
	{"none on create", validInput(), true, false, "", false},
	{"none on update", validInput(), false, false, "", false},
	{"full pair on create", withAction("actor", "pw"), true, false, "", false},
	{"empty pair on create is none", withAction("", ""), true, false, "", false},
	{"remove on update", withAction("", ""), false, false, "", false},
	{"username keeps stored password on update", withAction("actor", ""), false,
		false, "", false},
	{"username without password on create", withAction("actor", ""), true, true,
		"action_password", false},
	{"password without username", withAction("", "pw"), false, true,
		"action_username", false},
	{"password without username on create", withAction("", "pw"), true, true,
		"action_username", false},
	{"observation role reused", withAction("admin", "pw"), true, true,
		"observation role", true},
	{"observation role reused case-sensitively distinct", withAction("Admin", "pw"),
		true, false, "", false},
	{"blank username", withAction("   ", "pw"), true, true, "action_username", false},
}

func TestValidateActionInput(t *testing.T) {
	for _, tc := range actionInputCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateActionInput(tc.in, tc.create)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateActionInput = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrValidation) || !errors.Is(err, ErrActionIdentity) {
				t.Fatalf("err = %v, want ErrValidation and ErrActionIdentity", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err = %q, want it to name %q", err, tc.wantText)
			}
			if errors.Is(err, config.ErrActionDSNTarget) != tc.target {
				t.Errorf("ErrActionDSNTarget = %t, want %t", !tc.target, tc.target)
			}
			if strings.Contains(err.Error(), "pw") {
				t.Errorf("error echoes the password: %q", err)
			}
		})
	}
}

func TestOpenActionPassword_OpensOnlyForItsRowAndColumn(t *testing.T) {
	s := NewDatabaseStore(nil, unitKeyA)
	enc, err := s.sealActionPassword(7, "act-7")
	if err != nil {
		t.Fatalf("sealActionPassword: %v", err)
	}
	if strings.Contains(string(enc), "act-7") || !crypto.IsSealed(string(enc)) {
		t.Fatalf("sealed value %.30q holds the password or is not sage-enc:v1", enc)
	}
	pw, err := s.openActionPassword(7, enc)
	if err != nil || pw != "act-7" {
		t.Fatalf("openActionPassword = %q, %v", pw, err)
	}
	// Boundary: the same value copied to another row, or onto the
	// observation password column of the same row, does not open.
	if _, err := s.openActionPassword(8, enc); !errors.Is(err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("copied to row 8: err = %v, want ErrWrongEncryptionKey", err)
	}
	captureSlog(t)
	if pw, err := s.decryptPassword(t.Context(), 7, enc); err == nil || pw != "" {
		t.Fatalf("action value opened as the observation password: %q, %v", pw, err)
	}
}

func TestOpenActionPassword_WrongKeyNamesDatabase(t *testing.T) {
	logs := captureSlog(t)
	enc, err := NewDatabaseStore(nil, unitKeyA).sealActionPassword(5, "act-5")
	if err != nil {
		t.Fatal(err)
	}
	s := NewDatabaseStore(nil, unitKeyB)
	for i := 0; i < 3; i++ {
		pw, err := s.openActionPassword(5, enc)
		if !errors.Is(err, crypto.ErrWrongEncryptionKey) || pw != "" {
			t.Fatalf("openActionPassword under key B = %q, %v", pw, err)
		}
		if !strings.Contains(err.Error(), "database 5") ||
			!strings.Contains(err.Error(), "action password") {
			t.Fatalf("err = %q, want it to name database 5 and the action password", err)
		}
	}
	if n := strings.Count(logs.String(), "action password"); n != 1 {
		t.Fatalf("wrong-key log lines = %d, want 1:\n%s", n, logs.String())
	}
}

// Nil/empty: a value that is not sage-enc:v1, an empty value and a store
// with no key all refuse; nothing is returned in plaintext.
func TestOpenActionPassword_RefusesUnsealedEmptyAndKeyless(t *testing.T) {
	captureSlog(t)
	s := NewDatabaseStore(nil, unitKeyA)
	for _, enc := range [][]byte{nil, []byte("plain"), sealed(t, "raw", unitKeyA)} {
		if pw, err := s.openActionPassword(1, enc); err == nil || pw != "" {
			t.Errorf("openActionPassword(%.12q) = %q, %v; want an error", enc, pw, err)
		}
	}
	keyless := NewDatabaseStore(nil, nil)
	if _, err := keyless.sealActionPassword(1, "pw"); err == nil {
		t.Fatal("a store with no key sealed an action password")
	}
	if _, err := keyless.openActionPassword(1, []byte("sage-enc:v1:x")); !errors.Is(err,
		crypto.ErrWrongEncryptionKey) {
		t.Fatalf("keyless open: %v, want ErrWrongEncryptionKey", err)
	}
}
