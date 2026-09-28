package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// D7 account linking. An SSO identity (issuer+subject with a verified
// email) is bound to an existing account only by an explicit act: the
// signed-in user completing a link round trip, or the holder of an
// admin-issued one-time grant. Email equality is a precondition, never a
// trigger: accounts are never linked because their emails match.

var (
	// ErrOAuthLinkConflict is returned when the identity is already bound
	// to a user, the target user already has an identity, the user does
	// not exist, or the verified email does not match the account.
	ErrOAuthLinkConflict = errors.New(
		"oauth: identity cannot be linked to this account")
	// ErrOAuthNotLinked is returned when unlinking a user with no identity.
	ErrOAuthNotLinked = errors.New("oauth: account has no linked identity")
)

// Link intents record how a link round trip was started.
const (
	LinkViaSession = "session"
	LinkViaGrant   = "grant"
)

// LinkIntent is bound to an OAuth state when the round trip should link
// the returned identity to UserID instead of signing in.
type LinkIntent struct {
	UserID int
	Via    string
}

// LinkOAuthIdentity binds id to userID. It requires a verified email that
// equals the account email (case-insensitive) and refuses when either side
// is already linked, so an identity can never move between accounts.
// The password hash is kept: disabling password login is a separate act.
func LinkOAuthIdentity(
	ctx context.Context, pool *pgxpool.Pool,
	userID int, id Identity, provider string,
) error {
	if !id.EmailVerified {
		return ErrOAuthEmailUnverified
	}
	if id.Issuer == "" || id.Subject == "" || id.Email == "" {
		return fmt.Errorf("oauth: identity requires issuer, subject and email")
	}
	tag, err := pool.Exec(ctx,
		"UPDATE sage.users SET oauth_issuer = $1, oauth_subject = $2, "+
			"oauth_provider = $3 "+
			"WHERE id = $4 AND oauth_issuer IS NULL AND lower(email) = lower($5)",
		id.Issuer, id.Subject, provider, userID, id.Email)
	if isUniqueViolation(err) {
		return ErrOAuthLinkConflict
	}
	if err != nil {
		return fmt.Errorf("linking oauth identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOAuthLinkConflict
	}
	return nil
}

// UnlinkOAuthIdentity clears userID's SSO identity and ends its sessions,
// so a session minted through the removed identity cannot outlive it.
func UnlinkOAuthIdentity(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin unlink: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		"UPDATE sage.users SET oauth_issuer = NULL, oauth_subject = NULL, "+
			"oauth_provider = '' WHERE id = $1 AND oauth_issuer IS NOT NULL",
		userID)
	if err != nil {
		return fmt.Errorf("unlinking oauth identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return unlinkMissReason(ctx, tx, userID)
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM sage.sessions WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("ending sessions on unlink: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit unlink: %w", err)
	}
	return nil
}

func unlinkMissReason(ctx context.Context, tx pgx.Tx, userID int) error {
	var exists bool
	err := tx.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM sage.users WHERE id = $1)",
		userID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("checking user for unlink: %w", err)
	}
	if !exists {
		return ErrUserNotFound
	}
	return ErrOAuthNotLinked
}

// CreateSSOOnlyUser creates an account with no password. It is not bound
// to any provider, so a later SSO login with the same email is refused
// until the user is linked through an admin-issued grant.
func CreateSSOOnlyUser(
	ctx context.Context, pool *pgxpool.Pool, email, role string,
) (int, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return 0, fmt.Errorf("email required")
	}
	if !IsValidRole(role) {
		return 0, fmt.Errorf("invalid role: %q", role)
	}
	var id int
	err := pool.QueryRow(ctx,
		"INSERT INTO sage.users (email, password, role, oauth_provider) "+
			"VALUES ($1, NULL, $2, '') RETURNING id",
		email, role).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("creating sso-only user: %w", err)
	}
	return id, nil
}

// SSOStatus describes a user's sign-in methods.
type SSOStatus struct {
	Linked        bool
	Issuer        string
	PasswordLogin bool
}

// UserSSOStatus reports whether userID has a linked identity and a password.
func UserSSOStatus(
	ctx context.Context, pool *pgxpool.Pool, userID int,
) (SSOStatus, error) {
	var issuer *string
	var st SSOStatus
	err := pool.QueryRow(ctx,
		"SELECT oauth_issuer, password IS NOT NULL FROM sage.users WHERE id = $1",
		userID).Scan(&issuer, &st.PasswordLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOStatus{}, ErrUserNotFound
	}
	if err != nil {
		return SSOStatus{}, fmt.Errorf("reading sso status: %w", err)
	}
	if issuer != nil {
		st.Linked, st.Issuer = true, *issuer
	}
	return st, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
