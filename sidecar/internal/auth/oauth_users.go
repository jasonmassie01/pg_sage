package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FindOrCreateOAuthUser resolves an external identity to a local user.
// Users are matched on issuer+subject, never on email alone. A first
// login creates a new user unless the email already belongs to another
// account; such accounts are never auto-linked (ErrOAuthLinkRequired),
// except OAuth-only users created by the same provider before
// issuer/subject were recorded (G6-B04 / SURF-02).
func FindOrCreateOAuthUser(
	ctx context.Context, pool *pgxpool.Pool,
	id Identity, provider, defaultRole string,
) (*User, error) {
	if defaultRole == "" {
		defaultRole = RoleViewer
	}
	if !IsValidRole(defaultRole) {
		return nil, fmt.Errorf("invalid default role: %q", defaultRole)
	}
	if !id.EmailVerified {
		return nil, ErrOAuthEmailUnverified
	}
	if id.Issuer == "" || id.Subject == "" || id.Email == "" {
		return nil, fmt.Errorf("oauth: identity requires issuer, subject and email")
	}
	if u, err := userByOAuthIdentity(ctx, pool, id); err == nil {
		touchLastLogin(ctx, pool, u.ID)
		return u, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err := linkLegacyOAuthUser(ctx, pool, id, provider); err != nil {
		return nil, err
	}
	if u, err := userByOAuthIdentity(ctx, pool, id); err == nil {
		touchLastLogin(ctx, pool, u.ID)
		return u, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return createOAuthUser(ctx, pool, id, provider, defaultRole)
}

func userByOAuthIdentity(
	ctx context.Context, pool *pgxpool.Pool, id Identity,
) (*User, error) {
	var u User
	err := pool.QueryRow(ctx,
		"SELECT id, email, role, created_at, last_login "+
			"FROM sage.users WHERE oauth_issuer = $1 AND oauth_subject = $2",
		id.Issuer, id.Subject,
	).Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &u.LastLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("querying oauth identity: %w", err)
	}
	return &u, nil
}

// linkLegacyOAuthUser binds issuer+subject to an OAuth-only user row
// (no password, same provider, no identity yet) with this email.
func linkLegacyOAuthUser(
	ctx context.Context, pool *pgxpool.Pool, id Identity, provider string,
) error {
	_, err := pool.Exec(ctx,
		"UPDATE sage.users SET oauth_issuer = $1, oauth_subject = $2 "+
			"WHERE email = $3 AND password IS NULL "+
			"AND oauth_issuer IS NULL AND oauth_provider = $4",
		id.Issuer, id.Subject, id.Email, provider)
	if err != nil {
		return fmt.Errorf("linking legacy oauth user: %w", err)
	}
	return nil
}

func createOAuthUser(
	ctx context.Context, pool *pgxpool.Pool,
	id Identity, provider, role string,
) (*User, error) {
	u := User{Email: id.Email, Role: role}
	err := pool.QueryRow(ctx,
		"INSERT INTO sage.users "+
			"(email, role, oauth_provider, oauth_issuer, oauth_subject) "+
			"VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING "+
			"RETURNING id, created_at",
		id.Email, role, provider, id.Issuer, id.Subject,
	).Scan(&u.ID, &u.CreatedAt)
	if err == nil {
		return &u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("creating oauth user: %w", err)
	}
	// Conflict: either a concurrent first login for this identity won
	// the insert, or the email belongs to a different account.
	existing, err := userByOAuthIdentity(ctx, pool, id)
	if err == nil {
		return existing, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOAuthLinkRequired
	}
	return nil, err
}

func touchLastLogin(ctx context.Context, pool *pgxpool.Pool, userID int) {
	if _, err := pool.Exec(ctx,
		"UPDATE sage.users SET last_login = now() WHERE id = $1",
		userID); err != nil {
		slog.Warn("oauth: updating last_login failed",
			"user_id", userID, "error", err)
	}
}
