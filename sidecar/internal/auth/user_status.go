package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tokenRevokedByDeactivation is what sage.mcp_tokens.revoked_by records for
// an operator token revoked because its owner was deactivated.
const tokenRevokedByDeactivation = "pg_sage: owner deactivated"

// StatusChange reports what SetUserDisabled did.
type StatusChange struct {
	// Changed is false when the user already had the requested status.
	Changed bool
	// SessionsEnded and TokensRevoked count what a deactivation ended.
	SessionsEnded int
	TokensRevoked int
}

// SetUserDisabled deactivates or re-enables a user (apiauth-01).
// Deactivating ends every session of the user and revokes their operator
// MCP tokens in the same transaction; re-enabling restores sign-in only, so
// ended sessions and revoked tokens stay dead. Under the users-table lock
// that serializes role changes, it refuses to deactivate the last enabled
// admin (ErrLastAdmin). An unknown or non-positive id is ErrUserNotFound.
func SetUserDisabled(
	ctx context.Context, pool *pgxpool.Pool, userID int, disabled bool,
) (StatusChange, error) {
	if userID < 1 {
		return StatusChange{}, ErrUserNotFound
	}
	if pool == nil {
		return StatusChange{}, fmt.Errorf("setting user status: no database pool")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return StatusChange{}, fmt.Errorf("begin set user status: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE sage.users IN EXCLUSIVE MODE"); err != nil {
		return StatusChange{}, fmt.Errorf("locking users: %w", err)
	}
	res, err := setUserDisabledTx(ctx, tx, userID, disabled)
	if err != nil {
		return StatusChange{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return StatusChange{}, fmt.Errorf("commit set user status: %w", err)
	}
	return res, nil
}

func setUserDisabledTx(
	ctx context.Context, tx pgx.Tx, userID int, disabled bool,
) (StatusChange, error) {
	role, enabled, err := userRoleTx(ctx, tx, userID)
	if err != nil {
		return StatusChange{}, err
	}
	if enabled != disabled {
		return StatusChange{}, nil // already in the requested state
	}
	if disabled && role == RoleAdmin {
		count, err := countAdminsTx(ctx, tx)
		if err != nil {
			return StatusChange{}, err
		}
		if count <= 1 {
			return StatusChange{}, ErrLastAdmin
		}
	}
	if _, err := tx.Exec(ctx,
		"UPDATE sage.users SET disabled_at = CASE WHEN $2 THEN now() END WHERE id = $1",
		userID, disabled); err != nil {
		return StatusChange{}, fmt.Errorf("updating user status: %w", err)
	}
	res := StatusChange{Changed: true}
	if !disabled {
		return res, nil
	}
	if res.SessionsEnded, err = EndUserSessions(ctx, tx, userID, ""); err != nil {
		return StatusChange{}, fmt.Errorf("ending sessions of deactivated user: %w", err)
	}
	if res.TokensRevoked, err = revokeOwnedMCPTokens(ctx, tx, userID); err != nil {
		return StatusChange{}, err
	}
	return res, nil
}

// revokeOwnedMCPTokens revokes the live operator MCP tokens userID owns.
// The tokens table lives with the users table; a database without it (a
// minimal or pre-MCP schema) has no tokens to revoke.
func revokeOwnedMCPTokens(ctx context.Context, tx pgx.Tx, userID int) (int, error) {
	var present bool
	if err := tx.QueryRow(ctx,
		"SELECT to_regclass('sage.mcp_tokens') IS NOT NULL").Scan(&present); err != nil {
		return 0, fmt.Errorf("checking for MCP tokens: %w", err)
	}
	if !present {
		return 0, nil
	}
	tag, err := tx.Exec(ctx,
		"UPDATE sage.mcp_tokens SET revoked_at = now(), revoked_by = $2 "+
			"WHERE owner_user_id = $1 AND revoked_at IS NULL",
		userID, tokenRevokedByDeactivation)
	if err != nil {
		return 0, fmt.Errorf("revoking MCP tokens of deactivated user: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
