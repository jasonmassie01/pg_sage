package auth

import (
	"context"
	"errors"
	"testing"
)

// apiauth-03 review: a change must pick a new password. Keeping the current
// one would let a forced change keep the temporary password the admin saw
// (and sent over some channel), which defeats the reset.

// No concurrent access test here: the row lock under concurrent changes is
// covered by TestChangePassword_ConcurrentChangesOneWins.

func TestChangePassword_RefusesTheCurrentPasswordAsTheNewOne(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, pool, "reuse@example.com")
	s1, _ := CreateSession(ctx, pool, id)
	before := storedHash(t, pool, id)
	ended, err := ChangePassword(ctx, pool, id, pwOld, pwOld, "")
	if !errors.Is(err, ErrPasswordPolicy) || ended != 0 {
		t.Fatalf("same password: ended=%d err=%v, want 0 and ErrPasswordPolicy", ended, err)
	}
	if storedHash(t, pool, id) != before {
		t.Fatal("a refused same-password change rewrote the hash")
	}
	if _, err := ValidateSession(ctx, pool, s1); err != nil {
		t.Fatalf("a refused change ended a session: %v", err)
	}
	// A wrong current password stays a 401-class error even when next
	// equals it: the caller learns nothing about the real password.
	if _, err := ChangePassword(ctx, pool, id, pwNew, pwNew, ""); !errors.Is(err,
		ErrCurrentPasswordWrong) {
		t.Fatalf("wrong current equal to next: %v, want ErrCurrentPasswordWrong", err)
	}
}

func TestChangePassword_ForcedChangeCannotKeepTheTemporaryPassword(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, pool, "keep-temp@example.com")
	temp, _, err := ResetPassword(ctx, pool, id)
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := ChangePassword(ctx, pool, id, temp, temp, ""); !errors.Is(err,
		ErrPasswordPolicy) {
		t.Fatalf("keeping the temporary password: %v, want ErrPasswordPolicy", err)
	}
	if flag, _ := mustChangeFlag(t, pool, id); !flag {
		t.Fatal("a refused forced change cleared must_change_password")
	}
	if _, err := Authenticate(ctx, pool, "keep-temp@example.com", temp); err != nil {
		t.Fatalf("the temporary password stopped working after a refusal: %v", err)
	}
}
