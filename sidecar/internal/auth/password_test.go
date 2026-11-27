package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-03: one password policy (12 to 72 bytes, not the account email)
// for every path that sets a password, and change/reset operations that
// swap the hash and end sessions in one transaction.

func TestValidatePassword_Policy(t *testing.T) {
	const email = "Owner@Example.com"
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"empty", "", false},
		{"11 bytes", strings.Repeat("a", 11), false},
		{"12 bytes", strings.Repeat("a", 12), true},
		{"72 bytes", strings.Repeat("b", 72), true},
		{"73 bytes", strings.Repeat("b", 73), false},
		// Bytes, not runes: 4 three-byte runes are 12 bytes.
		{"12 bytes of multibyte runes", "日本語日", true},
		{"24 multibyte runes over 72 bytes", strings.Repeat("日", 25), false},
		{"the email", "Owner@Example.com", false},
		{"the email in another case", "owner@example.COM", false},
		{"the email with spaces", "  owner@example.com ", false},
		{"contains the email", "xowner@example.comx", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePassword(email, c.pw)
			if c.ok && err != nil {
				t.Fatalf("ValidatePassword(%q) = %v, want nil", c.pw, err)
			}
			if !c.ok && !errors.Is(err, ErrPasswordPolicy) {
				t.Fatalf("ValidatePassword(%q) = %v, want ErrPasswordPolicy", c.pw, err)
			}
		})
	}
}

func TestValidatePassword_MessageNeverEchoesThePassword(t *testing.T) {
	pw := "owner@example.com"
	err := ValidatePassword("owner@example.com", pw)
	if err == nil || strings.Contains(err.Error(), pw) {
		t.Fatalf("error %v must exist and not echo the password", err)
	}
	long := strings.Repeat("s3cr3t-", 11)
	err = ValidatePassword("a@b.io", long)
	if err == nil || strings.Contains(err.Error(), long) {
		t.Fatalf("error %v must exist and not echo the password", err)
	}
}

func TestValidatePassword_EmptyEmailSkipsTheEmailRule(t *testing.T) {
	if err := ValidatePassword("", "twelve-bytes"); err != nil {
		t.Fatalf("ValidatePassword with no email = %v, want nil", err)
	}
}

func TestCreateUser_RejectsOverBcryptLimitAsPolicy(t *testing.T) {
	// A nil pool would panic if CreateUser reached the database.
	_, err := CreateUser(context.Background(), nil, "a@b.io",
		strings.Repeat("x", 73), RoleViewer)
	if !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("CreateUser(73 bytes) = %v, want ErrPasswordPolicy", err)
	}
}

func TestPasswordOps_NilPoolAndBadIDs(t *testing.T) {
	ctx := context.Background()
	if _, err := ChangePassword(ctx, nil, 1, "a", "b", ""); err == nil {
		t.Fatal("ChangePassword with nil pool succeeded")
	}
	if _, _, err := ResetPassword(ctx, nil, 1); err == nil {
		t.Fatal("ResetPassword with nil pool succeeded")
	}
	if _, err := ChangePassword(ctx, nil, 0, "a", "b", ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("ChangePassword(id 0) = %v, want ErrUserNotFound", err)
	}
	if _, _, err := ResetPassword(ctx, nil, -3); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("ResetPassword(id -3) = %v, want ErrUserNotFound", err)
	}
}

const pwOld = "old-password-one"
const pwNew = "new-password-two"

func passwordUser(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	id, err := CreateUser(context.Background(), pool, email, pwOld, RoleViewer)
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return id
}

func storedHash(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(context.Background(),
		`SELECT password FROM sage.users WHERE id = $1`, id).Scan(&hash); err != nil {
		t.Fatalf("reading hash: %v", err)
	}
	return hash
}

func mustChangeFlag(t *testing.T, pool *pgxpool.Pool, id int) (bool, bool) {
	t.Helper()
	var flag, changedAt bool
	if err := pool.QueryRow(context.Background(),
		`SELECT must_change_password, password_changed_at IS NOT NULL
		   FROM sage.users WHERE id = $1`, id).Scan(&flag, &changedAt); err != nil {
		t.Fatalf("reading flag: %v", err)
	}
	return flag, changedAt
}

func TestChangePassword_SwapsHashAndEndsOtherSessions(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, pool, "change@example.com")
	keep, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	other, err := CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := ChangePassword(ctx, pool, id, pwOld, pwNew, HashSessionToken(keep))
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if ended != 1 {
		t.Fatalf("sessions ended = %d, want 1", ended)
	}
	if _, err := Authenticate(ctx, pool, "change@example.com", pwOld); !errors.Is(err,
		ErrInvalidCredentials) {
		t.Fatalf("old password login = %v, want ErrInvalidCredentials", err)
	}
	if u, err := Authenticate(ctx, pool, "change@example.com", pwNew); err != nil ||
		u.ID != id {
		t.Fatalf("new password login = %+v, %v", u, err)
	}
	if u, err := ValidateSession(ctx, pool, keep); err != nil || u.ID != id {
		t.Fatalf("calling session = %+v, %v; want it kept", u, err)
	}
	if _, err := ValidateSession(ctx, pool, other); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("other session = %v, want ErrSessionInvalid", err)
	}
	flag, changedAt := mustChangeFlag(t, pool, id)
	if flag || !changedAt {
		t.Fatalf("must_change=%v changed_at set=%v, want false/true", flag, changedAt)
	}
}

func TestChangePassword_RefusalsLeaveTheHash(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, pool, "refuse@example.com")
	before := storedHash(t, pool, id)
	cases := []struct {
		name, current, next string
		want                error
	}{
		{"wrong current", "not-the-password", pwNew, ErrCurrentPasswordWrong},
		{"empty current", "", pwNew, ErrCurrentPasswordWrong},
		{"short next", pwOld, "short-11byt", ErrPasswordPolicy},
		{"long next", pwOld, strings.Repeat("z", 73), ErrPasswordPolicy},
		{"next is email", pwOld, "REFUSE@example.com", ErrPasswordPolicy},
	}
	for _, c := range cases {
		if _, err := ChangePassword(ctx, pool, id, c.current, c.next, ""); !errors.Is(err,
			c.want) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if storedHash(t, pool, id) != before {
		t.Fatal("a refused change rewrote the hash")
	}
	if _, err := ChangePassword(ctx, pool, 999999, pwOld, pwNew, ""); !errors.Is(err,
		ErrUserNotFound) {
		t.Fatalf("unknown user: %v, want ErrUserNotFound", err)
	}
}

func TestChangePassword_SSOOnlyUserHasNoPassword(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id, err := CreateSSOOnlyUser(ctx, pool, "sso-only@example.com", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ChangePassword(ctx, pool, id, "", pwNew, ""); !errors.Is(err,
		ErrNoPassword) {
		t.Fatalf("ChangePassword(SSO-only) = %v, want ErrNoPassword", err)
	}
	if _, _, err := ResetPassword(ctx, pool, id); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("ResetPassword(SSO-only) = %v, want ErrNoPassword", err)
	}
	var hash *string
	if err := pool.QueryRow(ctx, `SELECT password FROM sage.users WHERE id = $1`,
		id).Scan(&hash); err != nil || hash != nil {
		t.Fatalf("SSO-only user gained a password: %v %v", hash, err)
	}
}

func TestResetPassword_TemporaryPasswordForcesChange(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, pool, "reset@example.com")
	s1, _ := CreateSession(ctx, pool, id)
	s2, _ := CreateSession(ctx, pool, id)
	temp, ended, err := ResetPassword(ctx, pool, id)
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if ended != 2 {
		t.Fatalf("sessions ended = %d, want 2", ended)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(temp)
	if err != nil || len(raw) != 24 {
		t.Fatalf("temporary password %d chars decodes to %d bytes (%v), want 24",
			len(temp), len(raw), err)
	}
	if err := ValidatePassword("reset@example.com", temp); err != nil {
		t.Fatalf("temporary password breaks the policy: %v", err)
	}
	for _, s := range []string{s1, s2} {
		if _, err := ValidateSession(ctx, pool, s); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("session after reset = %v, want ErrSessionInvalid", err)
		}
	}
	if _, err := Authenticate(ctx, pool, "reset@example.com", pwOld); err == nil {
		t.Fatal("the old password still works after a reset")
	}
	u, err := Authenticate(ctx, pool, "reset@example.com", temp)
	if err != nil || !u.MustChangePassword || !u.HasPassword {
		t.Fatalf("temp login = %+v, %v; want must_change and has_password", u, err)
	}
	sess, _ := CreateSession(ctx, pool, id)
	if v, err := ValidateSession(ctx, pool, sess); err != nil || !v.MustChangePassword {
		t.Fatalf("session user = %+v, %v; want MustChangePassword", v, err)
	}
	// The forced change clears the flag.
	if _, err := ChangePassword(ctx, pool, id, temp, pwNew, HashSessionToken(sess)); err != nil {
		t.Fatalf("change after reset: %v", err)
	}
	if v, err := ValidateSession(ctx, pool, sess); err != nil || v.MustChangePassword {
		t.Fatalf("after change = %+v, %v; want flag cleared", v, err)
	}
	other, _, err := ResetPassword(ctx, pool, id)
	if err != nil || other == temp {
		t.Fatalf("second reset = %v; temporary passwords must differ", err)
	}
}

func TestResetPassword_UnknownUser(t *testing.T) {
	pool := setupOIDCPool(t)
	if _, _, err := ResetPassword(context.Background(), pool, 999999); !errors.Is(err,
		ErrUserNotFound) {
		t.Fatalf("ResetPassword(unknown) = %v, want ErrUserNotFound", err)
	}
}

// A failure after the hash UPDATE (here: ending the sessions) rolls the
// whole change back, so the old password and every session survive.
func TestChangePassword_FailureMidTransactionKeepsOldState(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, pool, "midtx@example.com")
	before := storedHash(t, pool, id)
	keep, _ := CreateSession(ctx, pool, id)
	other, _ := CreateSession(ctx, pool, id)
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION sage.apiauth03_fail_delete() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
			RAISE EXCEPTION 'apiauth03 injected failure'; END $$;
		CREATE TRIGGER apiauth03_fail_delete BEFORE DELETE ON sage.sessions
			FOR EACH ROW EXECUTE FUNCTION sage.apiauth03_fail_delete();`); err != nil {
		t.Fatalf("installing failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS apiauth03_fail_delete ON sage.sessions;
			DROP FUNCTION IF EXISTS sage.apiauth03_fail_delete();`)
	})
	_, err := ChangePassword(ctx, pool, id, pwOld, pwNew, HashSessionToken(keep))
	if err == nil || !strings.Contains(err.Error(), "apiauth03 injected failure") {
		t.Fatalf("ChangePassword = %v, want the injected failure", err)
	}
	if _, _, err := ResetPassword(ctx, pool, id); err == nil {
		t.Fatal("ResetPassword succeeded through the injected failure")
	}
	if storedHash(t, pool, id) != before {
		t.Fatal("the hash changed although the transaction failed")
	}
	if flag, changedAt := mustChangeFlag(t, pool, id); flag || changedAt {
		t.Fatalf("flags changed on failure: must_change=%v changed_at=%v", flag, changedAt)
	}
	for _, s := range []string{keep, other} {
		if _, err := ValidateSession(ctx, pool, s); err != nil {
			t.Fatalf("session ended although the transaction failed: %v", err)
		}
	}
}

// Two changes racing with the same current password: the row lock makes
// the second see the first's hash, so exactly one succeeds.
func TestChangePassword_ConcurrentChangesOneWins(t *testing.T) {
	setup := setupOIDCPool(t)
	ctx := context.Background()
	id := passwordUser(t, setup, "race@example.com")
	pool, err := pgxpool.New(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = ChangePassword(ctx, pool, id, pwOld,
				[]string{"racer-one-password", "racer-two-password"}[i], "")
		}(i)
	}
	wg.Wait()
	wins, wrong := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrCurrentPasswordWrong):
			wrong++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || wrong != 1 {
		t.Fatalf("wins=%d wrong=%d, want 1 and 1 (%v)", wins, wrong, errs)
	}
}

func TestBootstrapAdmin_GeneratedPasswordForcesChange(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if err := BootstrapGeneratedAdmin(ctx, pool, "gen-admin@example.com",
		"generated-password-1"); err != nil {
		t.Fatalf("BootstrapGeneratedAdmin: %v", err)
	}
	u, err := Authenticate(ctx, pool, "gen-admin@example.com", "generated-password-1")
	if err != nil || !u.MustChangePassword || u.Role != RoleAdmin {
		t.Fatalf("generated admin = %+v, %v; want an admin who must change", u, err)
	}
	if err := BootstrapGeneratedAdmin(ctx, pool, "second@example.com",
		"generated-password-2"); err == nil {
		t.Fatal("a second bootstrap succeeded with users present")
	}
}

func TestBootstrapAdmin_SuppliedSecretDoesNotForceChange(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if err := BootstrapAdmin(ctx, pool, "sup-admin@example.com",
		"supplied-secret-1"); err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	u, err := Authenticate(ctx, pool, "sup-admin@example.com", "supplied-secret-1")
	if err != nil || u.MustChangePassword {
		t.Fatalf("supplied-secret admin = %+v, %v; want no forced change", u, err)
	}
}
