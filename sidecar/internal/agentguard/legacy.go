package agentguard

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LegacyProfile is the profile migrated agent tokens get (§6.4, §9:
// read only, ceiling prod).
const LegacyProfile = "legacy"

// LegacyCreatedBy marks principals created by the G1 token migration.
const LegacyCreatedBy = "migration:g1"

const (
	slugMax         = 54
	slugSuffixLen   = 4
	slugMaxAttempts = 20
)

// Slug is the §6.4 slug rule for a legacy token name: lowercase, any other
// character becomes "-", truncated to 54. A result that is not a valid
// name (it must start with a letter and have two characters) gets the
// prefix "agent-" first.
func Slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	if !ValidName(truncate(s, slugMax)) {
		s = "agent-" + s
	}
	return truncate(s, slugMax)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// MigratedToken is one legacy agent token bound to a new principal.
type MigratedToken struct {
	TokenID     string `json:"token_id"`
	TokenName   string `json:"token_name"`
	PrincipalID string `json:"principal_id"`
	Principal   string `json:"principal"`
}

// LegacyResult is what the migration did.
type LegacyResult struct {
	Migrated []MigratedToken `json:"migrated"`
	// AgentDBTokens counts live tokens of the removed AgentDB provisioner
	// (sage.agent_db_agent_tokens). Nothing authenticates them any more;
	// they are reported, not migrated, and go with the legacy tables.
	AgentDBTokens int `json:"agentdb_tokens"`
}

// MigrateLegacyTokens binds every live agent MCP token that has no
// principal to a new unsponsored principal with the legacy profile (G1-11).
// Read tools keep working, propose tools queue for a human (L2) and agent_*
// tools answer agent_unsponsored until an admin assigns a sponsor. It runs
// in one transaction under an advisory lock, so concurrent sidecars migrate
// each token once, and it is idempotent.
func MigrateLegacyTokens(ctx context.Context, pool *pgxpool.Pool) (LegacyResult, error) {
	if pool == nil {
		return LegacyResult{}, ErrUnavailable
	}
	var res LegacyResult
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage guard_legacy v1 */
			SELECT pg_advisory_xact_lock(hashtextextended('pg_sage guard_legacy', 0))`); err != nil {
			return err
		}
		tokens, err := unboundTokens(ctx, tx)
		if err != nil {
			return err
		}
		for _, t := range tokens {
			m, err := bindLegacyToken(ctx, tx, t)
			if err != nil {
				return err
			}
			res.Migrated = append(res.Migrated, m)
		}
		res.AgentDBTokens, err = countAgentDBTokens(ctx, tx)
		return err
	})
	if err != nil {
		return LegacyResult{}, fmt.Errorf("agentguard: migrating legacy agent tokens: %w", err)
	}
	return res, nil
}

type legacyToken struct{ id, name string }

func unboundTokens(ctx context.Context, tx pgx.Tx) ([]legacyToken, error) {
	rows, err := tx.Query(ctx, `/* pg_sage guard_legacy v1 */
		SELECT id::text, name FROM sage.mcp_tokens
		WHERE kind = 'agent' AND principal_id IS NULL AND revoked_at IS NULL
		  AND expires_at > now()
		ORDER BY created_at, id FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (legacyToken, error) {
		var t legacyToken
		return t, r.Scan(&t.id, &t.name)
	})
}

// bindLegacyToken creates the token's principal under a unique slug and
// binds the token to it. A taken name gets a random 4-character suffix.
func bindLegacyToken(ctx context.Context, tx pgx.Tx, t legacyToken) (MigratedToken, error) {
	id, err := NewID()
	if err != nil {
		return MigratedToken{}, err
	}
	base := Slug(t.name)
	name := base
	for attempt := 0; attempt < slugMaxAttempts; attempt++ {
		inserted, err := insertLegacyPrincipal(ctx, tx, id, name, t.id)
		if err != nil {
			return MigratedToken{}, err
		}
		if inserted {
			_, err = tx.Exec(ctx, `/* pg_sage guard_legacy v1 */
				UPDATE sage.mcp_tokens SET principal_id = $1 WHERE id = $2::uuid`, id, t.id)
			return MigratedToken{TokenID: t.id, TokenName: t.name, PrincipalID: id,
				Principal: name}, err
		}
		suffix, err := randomBase32(slugSuffixLen)
		if err != nil {
			return MigratedToken{}, err
		}
		name = base + "-" + suffix
	}
	return MigratedToken{}, fmt.Errorf("no free name for token %s after %d attempts", t.id,
		slugMaxAttempts)
}

// insertLegacyPrincipal inserts under a savepoint; false means the name
// is taken.
func insertLegacyPrincipal(ctx context.Context, tx pgx.Tx, id, name,
	tokenID string) (bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	_, err = sp.Exec(ctx, `/* pg_sage guard_legacy v1 */
		INSERT INTO sage.guard_principals (id, name, profile, env_ceiling, created_by)
		VALUES ($1, $2, $3, 'prod', $4)`, id, name, LegacyProfile,
		LegacyCreatedBy+" token "+tokenID)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == sqlUniqueViol {
		return false, sp.Rollback(ctx)
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		return false, err
	}
	return true, sp.Commit(ctx)
}

func countAgentDBTokens(ctx context.Context, tx pgx.Tx) (int, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `/* pg_sage guard_legacy v1 */
		SELECT to_regclass('sage.agent_db_agent_tokens') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var n int
	err := tx.QueryRow(ctx, `/* pg_sage guard_legacy v1 */
		SELECT count(*)::int FROM sage.agent_db_agent_tokens
		WHERE revoked_at IS NULL AND expires_at > now()`).Scan(&n)
	return n, err
}
