package mcpauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBResolver resolves identities through sage.guard_identity_bindings
// (spec §7) on the control database, skipping retired principals. Where
// the table does not exist (a schema older than G1) nothing is bound:
// every token is refused with ErrNoBinding (fail closed).
type DBResolver struct {
	pool *pgxpool.Pool
}

// NewDBResolver returns a resolver over pool.
func NewDBResolver(pool *pgxpool.Pool) *DBResolver { return &DBResolver{pool: pool} }

const resolveSQL = `/* pg_sage mcp_oauth v1 */ SELECT b.principal_id
	FROM sage.guard_identity_bindings b
	JOIN sage.guard_principals p ON p.id = b.principal_id
	WHERE b.issuer = $1 AND b.subject = $2 AND p.status <> 'retired'`

// PrincipalForSubject returns the principal bound to (issuer, subject).
func (r *DBResolver) PrincipalForSubject(ctx context.Context, issuer, subject string) (
	string, error) {
	if r == nil || r.pool == nil {
		return "", fmt.Errorf("%w: no control database", ErrNoBinding)
	}
	var id string
	err := r.pool.QueryRow(ctx, resolveSQL, issuer, subject).Scan(&id)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrNoBinding
	case errors.As(err, &pgErr) && pgErr.Code == "42P01":
		return "", fmt.Errorf("%w: identity bindings are not installed",
			ErrNoBinding)
	case err != nil:
		return "", fmt.Errorf("resolve identity binding: %w", err)
	}
	return id, nil
}
