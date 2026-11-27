package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidRole is returned by UpdateUserRole when the supplied
// role value is not one of the recognized roles. Handlers should
// surface this as a 400 client error.
var ErrInvalidRole = errors.New("invalid role")

// ErrUserNotFound is returned when an operation targets a user ID
// that does not exist. Handlers should surface this as a 404
// client error rather than a 500.
var ErrUserNotFound = errors.New("user not found")

// ErrInvalidCredentials is returned by Authenticate for an unknown email,
// an account without a password, or a wrong password. Callers must not
// tell these apart in a response.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrLastAdmin is returned when a user mutation would leave the
// system without any enabled admin account.
var ErrLastAdmin = errors.New("cannot remove the last admin")

// ErrUserDisabled is returned when a deactivated account tries to sign in
// through SSO. Handlers surface it as 403. Password sign-in never reveals
// it (ErrInvalidCredentials).
var ErrUserDisabled = errors.New("user account is deactivated")

// HashPassword returns a bcrypt hash of the password at BcryptCost.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword(
		[]byte(password), int(hashCost.Load()),
	)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(bytes), nil
}

// CheckPassword returns true if password matches the hash.
func CheckPassword(hash, password string) bool {
	err := bcrypt.CompareHashAndPassword(
		[]byte(hash), []byte(password),
	)
	return err == nil
}

// CreateUser inserts a new user and returns the user ID. It is the storage
// primitive: callers that take a password from a person (the users API,
// bootstrap, change and reset) apply ValidatePassword first. CreateUser
// itself refuses only what bcrypt cannot store safely (over 72 bytes) and
// the historical 8-byte floor, both as ErrPasswordPolicy.
func CreateUser(
	ctx context.Context, pool *pgxpool.Pool,
	email, password, role string,
) (int, error) {
	return createUser(ctx, pool, email, password, role, false)
}

func createUser(
	ctx context.Context, pool *pgxpool.Pool,
	email, password, role string, mustChange bool,
) (int, error) {
	if len(password) < 8 || len(password) > MaxPasswordBytes {
		return 0, fmt.Errorf("%w: a password must be at least 8 characters "+
			"and at most %d bytes", ErrPasswordPolicy, MaxPasswordBytes)
	}
	if !IsValidRole(role) {
		return 0, fmt.Errorf("invalid role: %q", role)
	}
	email = NormalizeEmail(email)
	if email == "" {
		return 0, fmt.Errorf("email required")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	var id int
	err = pool.QueryRow(ctx,
		"INSERT INTO sage.users (email, password, role, must_change_password) "+
			"SELECT $1::text, $2::text, $3::text, $4::boolean "+newEmailOnlySQL+
			" RETURNING id",
		email, hash, role, mustChange,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("creating user: %w", ErrEmailInUse)
	}
	if err != nil {
		return 0, fmt.Errorf("creating user: %w", err)
	}
	return id, nil
}

// Authenticate verifies credentials and returns the user. The email
// matches case-insensitively; one that matches several accounts (case
// variants from before apiauth-05) is refused like an unknown one.
func Authenticate(
	ctx context.Context, pool *pgxpool.Pool,
	email, password string,
) (*User, error) {
	u, hash, err := passwordAccount(ctx, pool, email)
	if errors.Is(err, errNoPasswordAccount) {
		// No password to check (unknown, ambiguous, SSO-only or
		// deactivated): pay the same bcrypt cost anyway, so the response
		// time does not reveal which emails have accounts.
		return nil, equalizeRefusalTiming(password)
	}
	if err != nil {
		return nil, err
	}
	if !CheckPassword(hash, password) {
		return nil, ErrInvalidCredentials
	}
	_, err = pool.Exec(ctx,
		"UPDATE sage.users SET last_login = now() WHERE id = $1",
		u.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("updating last_login: %w", err)
	}
	return u, nil
}

// errNoPasswordAccount: no single enabled account with a password has
// this email.
var errNoPasswordAccount = errors.New("no password account for email")

// passwordAccount returns the one account with this email (any case) and
// its password hash, or errNoPasswordAccount.
func passwordAccount(
	ctx context.Context, pool *pgxpool.Pool, email string,
) (*User, string, error) {
	rows, err := pool.Query(ctx,
		"SELECT id, email, password, role, created_at, last_login, "+
			"must_change_password, disabled_at IS NOT NULL FROM sage.users "+
			"WHERE lower(email) = lower($1) ORDER BY id LIMIT 2",
		NormalizeEmail(email))
	if err != nil {
		return nil, "", fmt.Errorf("querying user: %w", err)
	}
	defer rows.Close()
	var found []User
	var hash *string
	var disabled bool
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &hash, &u.Role,
			&u.CreatedAt, &u.LastLogin, &u.MustChangePassword, &disabled); err != nil {
			return nil, "", fmt.Errorf("querying user: %w", err)
		}
		found = append(found, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("querying user: %w", err)
	}
	if len(found) > 1 {
		logAmbiguousEmail("password", []int{found[0].ID, found[1].ID})
	}
	if len(found) != 1 || hash == nil || disabled {
		return nil, "", errNoPasswordAccount
	}
	found[0].HasPassword = true
	return &found[0], *hash, nil
}

// CreateSession inserts a session row and returns its cookie value.
func CreateSession(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) (string, error) {
	return CreateSessionWithDuration(ctx, pool, userID, SessionDuration)
}

// ValidateSession returns the enabled user of an unexpired session, looked
// up by the cookie's hash. It returns ErrSessionInvalid for an unknown,
// expired or malformed cookie, or a deactivated user (a malformed one is
// never queried), and ErrSessionStoreUnavailable, wrapping the cause, when
// there is no store or it fails to answer.
func ValidateSession(
	ctx context.Context, pool *pgxpool.Pool, token string,
) (*User, error) {
	hash, ok := sessionTokenHash(token)
	if !ok {
		return nil, ErrSessionInvalid
	}
	if pool == nil {
		return nil, fmt.Errorf("validating session: %w: no database pool",
			ErrSessionStoreUnavailable)
	}
	var u User
	err := pool.QueryRow(ctx,
		"/* pg_sage auth_validate_session v3 */ "+
			"SELECT u.id, u.email, u.role, u.created_at, u.last_login, "+
			"u.password IS NOT NULL, u.must_change_password "+
			"FROM sage.sessions s "+
			"JOIN sage.users u ON u.id = s.user_id "+
			"WHERE s.token_hash = $1 AND s.expires_at > now() "+
			"AND u.disabled_at IS NULL",
		hash,
	).Scan(&u.ID, &u.Email, &u.Role,
		&u.CreatedAt, &u.LastLogin, &u.HasPassword, &u.MustChangePassword)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, fmt.Errorf("validating session: %w: %w",
			ErrSessionStoreUnavailable, err)
	}
	return &u, nil
}

// DeleteSession removes the session a cookie names (logout). A malformed
// cookie names no session, so there is nothing to delete.
func DeleteSession(
	ctx context.Context, pool *pgxpool.Pool, token string,
) error {
	hash, ok := sessionTokenHash(token)
	if !ok {
		return nil
	}
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.sessions WHERE token_hash = $1", hash,
	)
	if err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// CleanExpiredSessions deletes all expired sessions.
func CleanExpiredSessions(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	if pool == nil {
		return fmt.Errorf("cleaning expired sessions: no database pool")
	}
	_, err := pool.Exec(ctx,
		"DELETE FROM sage.sessions WHERE expires_at <= now()",
	)
	if err != nil {
		return fmt.Errorf("cleaning expired sessions: %w", err)
	}
	return nil
}

// ListedUser is a user row for the admin list, with its sign-in methods.
type ListedUser struct {
	User
	SSOIssuer     string
	PasswordLogin bool
	DisabledAt    *time.Time
}

// ListUsers returns all users without password hashes, reporting whether
// each has a linked SSO identity and a password.
func ListUsers(
	ctx context.Context, pool *pgxpool.Pool,
) ([]ListedUser, error) {
	rows, err := pool.Query(ctx,
		"SELECT id, email, role, created_at, last_login, "+
			"COALESCE(oauth_issuer, ''), password IS NOT NULL, disabled_at "+
			"FROM sage.users ORDER BY id",
	)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()

	var users []ListedUser
	for rows.Next() {
		var u ListedUser
		if err := rows.Scan(
			&u.ID, &u.Email, &u.Role,
			&u.CreatedAt, &u.LastLogin,
			&u.SSOIssuer, &u.PasswordLogin, &u.DisabledAt,
		); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating users: %w", err)
	}
	return users, nil
}

// DeleteUserPreservingAdmin removes a user while atomically ensuring
// at least one admin remains. It serializes user-role mutations with
// a table lock so concurrent demote/delete requests cannot both pass
// a stale admin count.
func DeleteUserPreservingAdmin(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE sage.users IN EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("locking users: %w", err)
	}

	role, enabled, err := userRoleTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if role == RoleAdmin && enabled {
		count, err := countAdminsTx(ctx, tx)
		if err != nil {
			return err
		}
		if count <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM sage.users WHERE id = $1", userID,
	); err != nil {
		return fmt.Errorf("deleting user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete user: %w", err)
	}
	return nil
}

// UpdateUserRolePreservingAdmin changes a user's role while
// atomically preventing demotion of the last admin.
func UpdateUserRolePreservingAdmin(
	ctx context.Context, pool *pgxpool.Pool,
	userID int, role string,
) error {
	if !IsValidRole(role) {
		return fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin update user role: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE sage.users IN EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("locking users: %w", err)
	}

	currentRole, enabled, err := userRoleTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if currentRole == RoleAdmin && enabled && role != RoleAdmin {
		count, err := countAdminsTx(ctx, tx)
		if err != nil {
			return err
		}
		if count <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec(ctx,
		"UPDATE sage.users SET role = $1 WHERE id = $2",
		role, userID,
	); err != nil {
		return fmt.Errorf("updating user role: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit update user role: %w", err)
	}
	return nil
}

// userRoleTx reads a user's role and whether the account is enabled.
func userRoleTx(ctx context.Context, tx pgx.Tx, userID int) (string, bool, error) {
	var role string
	var enabled bool
	err := tx.QueryRow(ctx,
		"SELECT role, disabled_at IS NULL FROM sage.users WHERE id = $1",
		userID,
	).Scan(&role, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrUserNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("getting user role: %w", err)
	}
	return role, enabled, nil
}

// countAdminsTx counts enabled admins: a deactivated admin holds no power,
// so it never satisfies the last-admin rule.
func countAdminsTx(ctx context.Context, tx pgx.Tx) (int, error) {
	var count int
	err := tx.QueryRow(ctx,
		"SELECT count(*) FROM sage.users WHERE role = $1 AND disabled_at IS NULL",
		RoleAdmin,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting admins: %w", err)
	}
	return count, nil
}

// UserCount returns the total number of users.
func UserCount(
	ctx context.Context, pool *pgxpool.Pool,
) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sage.users",
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return count, nil
}

// BootstrapAdmin creates the first admin, from an operator-supplied secret,
// if no users exist. The operator chose the password, so it is not forced
// to change.
func BootstrapAdmin(
	ctx context.Context, pool *pgxpool.Pool,
	email, password string,
) error {
	return bootstrapAdmin(ctx, pool, email, password, false)
}

// BootstrapGeneratedAdmin creates the first admin with a password pg_sage
// generated and printed; the first sign-in must replace it (apiauth-03).
func BootstrapGeneratedAdmin(
	ctx context.Context, pool *pgxpool.Pool,
	email, password string,
) error {
	return bootstrapAdmin(ctx, pool, email, password, true)
}

func bootstrapAdmin(
	ctx context.Context, pool *pgxpool.Pool,
	email, password string, mustChange bool,
) error {
	count, err := UserCount(ctx, pool)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("admin bootstrap rejected: users already exist")
	}
	_, err = createUser(ctx, pool, email, password, RoleAdmin, mustChange)
	return err
}
