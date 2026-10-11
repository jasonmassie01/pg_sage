package decommission

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// LegacyTokensTable is the removed provisioner's agent token table
// (sage.agent_db_agent_tokens); it goes with the other legacy tables.
const LegacyTokensTable = "agent_db_agent_tokens"

// RowQuerier is the one method CountLiveLegacyTokens needs (a pool, a
// connection or a transaction).
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CountLiveLegacyTokens counts the removed provisioner's live agent tokens
// (not revoked, not expired). Nothing authenticates them any more; agent
// governance reports them at startup. Zero when the table is gone.
func CountLiveLegacyTokens(ctx context.Context, q RowQuerier) (int, error) {
	var exists bool
	if err := q.QueryRow(ctx, `/* pg_sage guard_legacy v1 */
		SELECT to_regclass('sage.agent_db_agent_tokens') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var n int
	err := q.QueryRow(ctx, `/* pg_sage guard_legacy v1 */
		SELECT count(*)::int FROM sage.agent_db_agent_tokens
		WHERE revoked_at IS NULL AND expires_at > now()`).Scan(&n)
	return n, err
}
