package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LinkGrantTTL bounds how long an admin-issued link grant stays usable.
const LinkGrantTTL = 15 * time.Minute

// ErrLinkGrantInvalid covers unknown, used, superseded and expired grants
// alike, so a caller learns nothing about which grants exist.
var ErrLinkGrantInvalid = errors.New("oauth: invalid or expired link grant")

// IssueLinkGrant creates a one-time grant that lets the holder link an SSO
// identity to userID without a password session. Only the SHA-256 hash is
// stored; the token is returned once. Issuing supersedes the user's unused
// grants. A user that is already linked cannot get a grant.
func IssueLinkGrant(
	ctx context.Context, pool *pgxpool.Pool, userID, issuedBy int,
) (string, time.Time, error) {
	token, err := newGrantToken()
	if err != nil {
		return "", time.Time{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("begin link grant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUnlinkedUser(ctx, tx, userID); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM sage.user_oidc_link_grants "+
			"WHERE user_id = $1 AND used_at IS NULL", userID); err != nil {
		return "", time.Time{}, fmt.Errorf("superseding link grants: %w", err)
	}
	var expires time.Time
	err = tx.QueryRow(ctx,
		"INSERT INTO sage.user_oidc_link_grants "+
			"(user_id, token_hash, created_by, expires_at) "+
			"VALUES ($1, $2, $3, now() + make_interval(secs => $4)) "+
			"RETURNING expires_at",
		userID, hashGrant(token), issuedBy, LinkGrantTTL.Seconds(),
	).Scan(&expires)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("storing link grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, fmt.Errorf("commit link grant: %w", err)
	}
	return token, expires, nil
}

func lockUnlinkedUser(ctx context.Context, tx pgx.Tx, userID int) error {
	var linked bool
	err := tx.QueryRow(ctx,
		"SELECT oauth_issuer IS NOT NULL FROM sage.users "+
			"WHERE id = $1 FOR UPDATE", userID).Scan(&linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("reading user for link grant: %w", err)
	}
	if linked {
		return ErrOAuthLinkConflict
	}
	return nil
}

// RedeemLinkGrant consumes a grant and returns the user it was issued for.
// A single conditional UPDATE makes redemption exactly-once.
func RedeemLinkGrant(
	ctx context.Context, pool *pgxpool.Pool, token string,
) (int, error) {
	if token == "" {
		return 0, ErrLinkGrantInvalid
	}
	var userID int
	err := pool.QueryRow(ctx,
		"UPDATE sage.user_oidc_link_grants SET used_at = now() "+
			"WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now() "+
			"RETURNING user_id", hashGrant(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLinkGrantInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("redeeming link grant: %w", err)
	}
	return userID, nil
}

func newGrantToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating link grant: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashGrant(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
