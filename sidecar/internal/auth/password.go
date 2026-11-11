package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Password policy (apiauth-03): every path that takes a password from a
// person (the users API, bootstrap, change and reset) applies it.
const (
	MinPasswordBytes = 12
	// MaxPasswordBytes is bcrypt's limit: longer input is refused rather
	// than silently truncated or failed at hashing time.
	MaxPasswordBytes = 72
	// temporaryPasswordBytes of randomness make a reset's temporary
	// password (base64url, 32 characters).
	temporaryPasswordBytes = 24
)

// ErrPasswordPolicy is a password the policy refuses. Handlers answer 400
// with the error text, which never contains the password.
var ErrPasswordPolicy = errors.New("password does not meet the policy")

// ErrNoPassword is an account without a password (SSO-only): there is
// nothing to change or reset. Handlers answer 409.
var ErrNoPassword = errors.New("account has no password")

// ErrCurrentPasswordWrong is a change whose current password does not
// match. Handlers answer 401 and count it against the login limiter.
var ErrCurrentPasswordWrong = errors.New("current password is incorrect")

// ValidatePassword checks pw against the policy for the account email:
// 12 to 72 bytes, and not the email itself in any case. An empty email
// skips the email rule.
func ValidatePassword(email, pw string) error {
	switch {
	case len(pw) < MinPasswordBytes:
		return fmt.Errorf("%w: use at least %d bytes", ErrPasswordPolicy, MinPasswordBytes)
	case len(pw) > MaxPasswordBytes:
		return fmt.Errorf("%w: use at most %d bytes", ErrPasswordPolicy, MaxPasswordBytes)
	}
	email = strings.TrimSpace(email)
	if email != "" && strings.EqualFold(strings.TrimSpace(pw), email) {
		return fmt.Errorf("%w: it must not be the account email", ErrPasswordPolicy)
	}
	return nil
}

// ChangePassword replaces userID's password after checking the current one
// and ends the user's other sessions (all but keepSessionHash, the stored
// hash of the calling session; empty ends them all). It clears
// must_change_password and refuses keeping the current password. The row
// lock makes concurrent changes with the same current password serialize:
// the second sees the new hash and gets ErrCurrentPasswordWrong. It
// returns how many sessions it ended.
func ChangePassword(
	ctx context.Context, pool *pgxpool.Pool,
	userID int, current, next, keepSessionHash string,
) (int, error) {
	if userID < 1 {
		return 0, ErrUserNotFound
	}
	if pool == nil {
		return 0, fmt.Errorf("changing password for user %d: no database pool", userID)
	}
	var ended int
	err := passwordTx(ctx, pool, userID, func(tx pgx.Tx, email, hash string) error {
		if err := ValidatePassword(email, next); err != nil {
			return err
		}
		if !CheckPassword(hash, current) {
			return ErrCurrentPasswordWrong
		}
		// current matched the hash, so next == current is the same password:
		// a forced change must not keep the temporary one the admin saw.
		if next == current {
			return fmt.Errorf("%w: choose a password different from the current password",
				ErrPasswordPolicy)
		}
		var err error
		ended, err = setPasswordTx(ctx, tx, userID, next, false, keepSessionHash)
		return err
	})
	if err != nil {
		return 0, err
	}
	return ended, nil
}

// ResetPassword gives userID a random temporary password, returned once and
// never stored in clear, sets must_change_password and ends every session
// of the user. It returns the temporary password and the sessions ended.
func ResetPassword(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) (string, int, error) {
	if userID < 1 {
		return "", 0, ErrUserNotFound
	}
	if pool == nil {
		return "", 0, fmt.Errorf("resetting password for user %d: no database pool", userID)
	}
	temp, err := temporaryPassword()
	if err != nil {
		return "", 0, err
	}
	var ended int
	err = passwordTx(ctx, pool, userID, func(tx pgx.Tx, _, _ string) error {
		var err error
		ended, err = setPasswordTx(ctx, tx, userID, temp, true, "")
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return temp, ended, nil
}

func temporaryPassword() (string, error) {
	b := make([]byte, temporaryPasswordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a temporary password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// passwordTx runs fn in one transaction with userID's row locked, passing
// the account email and password hash. An unknown user is ErrUserNotFound,
// an SSO-only one ErrNoPassword. fn's error rolls everything back.
func passwordTx(
	ctx context.Context, pool *pgxpool.Pool, userID int,
	fn func(tx pgx.Tx, email, hash string) error,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password update for user %d: %w", userID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var email string
	var hash *string
	err = tx.QueryRow(ctx,
		"/* pg_sage auth_password_lock v1 */ "+
			"SELECT email, password FROM sage.users WHERE id = $1 FOR UPDATE",
		userID).Scan(&email, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("locking user %d for a password update: %w", userID, err)
	}
	if hash == nil {
		return ErrNoPassword
	}
	if err := fn(tx, email, *hash); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit password update for user %d: %w", userID, err)
	}
	return nil
}

// setPasswordTx stores the hash of pw with the must-change flag and ends
// the user's sessions except keepSessionHash.
func setPasswordTx(
	ctx context.Context, tx pgx.Tx, userID int,
	pw string, mustChange bool, keepSessionHash string,
) (int, error) {
	hash, err := HashPassword(pw)
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx,
		"/* pg_sage auth_password_set v1 */ "+
			"UPDATE sage.users SET password = $2, must_change_password = $3, "+
			"password_changed_at = now() WHERE id = $1 AND password IS NOT NULL",
		userID, hash, mustChange)
	if err != nil {
		return 0, fmt.Errorf("storing the new password for user %d: %w", userID, err)
	}
	if tag.RowsAffected() != 1 {
		return 0, ErrNoPassword
	}
	ended, err := EndUserSessions(ctx, tx, userID, keepSessionHash)
	if err != nil {
		return 0, fmt.Errorf("ending sessions after a password update: %w", err)
	}
	return ended, nil
}
