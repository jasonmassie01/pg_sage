package mcptoken

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errNoPool is a storage failure, deliberately not a validation error.
var errNoPool = errors.New("mcptoken: no control database configured")

var uuidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Store persists tokens in sage.mcp_tokens.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a store on the control database. A nil pool yields a
// store that validates requests and fails every storage operation.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const tokenColumns = `id::text, name, kind, scopes, databases, owner_user_id, created_by,
	created_at, expires_at, revoked_at, revoked_by, last_used_at, prefix,
	COALESCE(principal_id, '')`

func scanToken(row pgx.Row) (Token, error) {
	var tok Token
	var kind string
	var revokedBy *string
	err := row.Scan(&tok.ID, &tok.Name, &kind, &tok.Scopes, &tok.Databases,
		&tok.OwnerUserID, &tok.CreatedBy, &tok.CreatedAt, &tok.ExpiresAt,
		&tok.RevokedAt, &revokedBy, &tok.LastUsedAt, &tok.Prefix, &tok.PrincipalID)
	if err != nil {
		return Token{}, err
	}
	tok.Kind = Kind(kind)
	if revokedBy != nil {
		tok.RevokedBy = *revokedBy
	}
	return tok, nil
}

// Pool is the control database pool (nil without one).
func (s *Store) Pool() *pgxpool.Pool {
	if s == nil {
		return nil
	}
	return s.pool
}

func (s *Store) ready() error {
	if s == nil || s.pool == nil {
		return errNoPool
	}
	return nil
}

// Create validates req and issues a token. The returned Token carries the
// plaintext Secret; it is never stored or returned again.
func (s *Store) Create(ctx context.Context, req CreateRequest) (Token, error) {
	req, err := normalize(req)
	if err != nil {
		return Token{}, err
	}
	if err := s.ready(); err != nil {
		return Token{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return Token{}, err
	}
	var owner *int
	var principal *string
	if req.Kind == KindOperator {
		owner = &req.OwnerUserID
	} else {
		principal = &req.PrincipalID
	}
	// The owner's role and the principal's status are checked in the same
	// statement that inserts, so a concurrent demotion or retirement cannot
	// slip between check and insert.
	row := s.pool.QueryRow(ctx, `/* pg_sage mcp_token_create v1 */
		INSERT INTO sage.mcp_tokens (name, kind, scopes, databases, token_hash, prefix,
			owner_user_id, created_by, expires_at, principal_id)
		SELECT $1::text, $2::text, $3::text[], $4::text[], $5::text, $6::text, $7::integer,
			$8::text, now() + make_interval(secs => $9::float8), $10::text
		WHERE ($7::integer IS NULL OR EXISTS (SELECT 1 FROM sage.users u
			WHERE u.id = $7::integer AND u.role IN ('operator', 'admin')))
		  AND ($10::text IS NULL OR EXISTS (SELECT 1 FROM sage.guard_principals p
			WHERE p.id = $10::text AND p.status <> 'retired'))
		RETURNING `+tokenColumns,
		req.Name, string(req.Kind), req.Scopes, req.Databases, HashSecret(secret),
		secret[:prefixLen], owner, req.CreatedBy, req.ExpiresIn.Seconds(), principal)
	tok, err := scanToken(row)
	if errors.Is(err, pgx.ErrNoRows) && req.Kind == KindAgent {
		return Token{}, ErrPrincipalRequired
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrOwnerRequired
	}
	if err != nil {
		return Token{}, fmt.Errorf("mcptoken: inserting token %q: %w", req.Name, err)
	}
	tok.Secret = secret
	return tok, nil
}

// List returns every token, newest first, without secrets or hashes.
func (s *Store) List(ctx context.Context) ([]Token, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+tokenColumns+`
		FROM sage.mcp_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("mcptoken: listing tokens: %w", err)
	}
	defer rows.Close()
	tokens := []Token{}
	for rows.Next() {
		tok, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("mcptoken: reading token row: %w", err)
		}
		tokens = append(tokens, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcptoken: listing tokens: %w", err)
	}
	return tokens, nil
}

// Revoke revokes token id on behalf of by. Revoking a revoked token is a
// no-op that returns it with its original revocation.
func (s *Store) Revoke(ctx context.Context, id, by string) (Token, error) {
	if !uuidPattern.MatchString(id) {
		return Token{}, ErrNotFound
	}
	if err := checkText("revoked_by", by, maxActorLen); err != nil {
		return Token{}, err
	}
	if err := s.ready(); err != nil {
		return Token{}, err
	}
	row := s.pool.QueryRow(ctx, `/* pg_sage */
		UPDATE sage.mcp_tokens
		SET revoked_at = COALESCE(revoked_at, now()),
			revoked_by = CASE WHEN revoked_at IS NULL THEN $2 ELSE revoked_by END
		WHERE id = $1::uuid
		RETURNING `+tokenColumns, id, by)
	tok, err := scanToken(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("mcptoken: revoking token %s: %w", id, err)
	}
	return tok, nil
}
