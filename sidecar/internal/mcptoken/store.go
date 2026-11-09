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

// scanToken reads tokenColumns, then any extra columns into extra.
func scanToken(row pgx.Row, extra ...any) (Token, error) {
	var tok Token
	var kind string
	var revokedBy *string
	dest := append([]any{&tok.ID, &tok.Name, &kind, &tok.Scopes, &tok.Databases,
		&tok.OwnerUserID, &tok.CreatedBy, &tok.CreatedAt, &tok.ExpiresAt,
		&tok.RevokedAt, &revokedBy, &tok.LastUsedAt, &tok.Prefix, &tok.PrincipalID},
		extra...)
	if err := row.Scan(dest...); err != nil {
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
	var tok Token
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var txErr error
		if tok, txErr = insertToken(ctx, tx, req, secret, owner, principal); txErr != nil {
			return txErr
		}
		return recordCreated(ctx, tx, req.Audit, tok)
	})
	if err != nil {
		return Token{}, err
	}
	tok.Secret = secret
	return tok, nil
}

// insertToken inserts the token row. The owner's role and the principal's
// status are checked in the same statement that inserts, so a concurrent
// demotion or retirement cannot slip between check and insert. A
// deactivated owner gets no token (apiauth-01); Validate refuses one
// regardless.
func insertToken(ctx context.Context, tx pgx.Tx, req CreateRequest, secret string,
	owner *int, principal *string) (Token, error) {
	row := tx.QueryRow(ctx, `/* pg_sage mcp_token_create v1 */
		INSERT INTO sage.mcp_tokens (name, kind, scopes, databases, token_hash, prefix,
			owner_user_id, created_by, expires_at, principal_id)
		SELECT $1::text, $2::text, $3::text[], $4::text[], $5::text, $6::text, $7::integer,
			$8::text, now() + make_interval(secs => $9::float8), $10::text
		WHERE ($7::integer IS NULL OR EXISTS (SELECT 1 FROM sage.users u
			WHERE u.id = $7::integer AND u.role IN ('operator', 'admin')
			  AND u.disabled_at IS NULL))
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
	return tok, nil
}

// List returns every token, newest first, without secrets or hashes. An
// agent token carries its principal's status: Validate refuses a retired
// agent's token although the token is neither revoked nor expired.
func (s *Store) List(ctx context.Context) ([]Token, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage mcp_token_list v2 */
		SELECT `+tokenColumns+`, principal_status FROM (
			SELECT t.*, COALESCE(p.status, '') AS principal_status
			FROM sage.mcp_tokens t
			LEFT JOIN sage.guard_principals p ON p.id = t.principal_id) t
		ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("mcptoken: listing tokens: %w", err)
	}
	defer rows.Close()
	tokens := []Token{}
	for rows.Next() {
		var principalStatus string
		tok, err := scanToken(rows, &principalStatus)
		if err != nil {
			return nil, fmt.Errorf("mcptoken: reading token row: %w", err)
		}
		tok.PrincipalStatus = principalStatus
		tokens = append(tokens, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcptoken: listing tokens: %w", err)
	}
	return tokens, nil
}

// Revoke revokes token id on behalf of by and records mcp_token_revoked
// in the same transaction. Revoking a revoked token is a no-op that
// returns it with its original revocation and records nothing.
func (s *Store) Revoke(ctx context.Context, id, by string, a Audit) (Token, error) {
	if !uuidPattern.MatchString(id) {
		return Token{}, ErrNotFound
	}
	if err := checkText("revoked_by", by, maxActorLen); err != nil {
		return Token{}, err
	}
	if err := a.validate(); err != nil {
		return Token{}, err
	}
	if err := s.ready(); err != nil {
		return Token{}, err
	}
	var tok Token
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var txErr error
		tok, txErr = revokeInTx(ctx, tx, id, by, a)
		return txErr
	})
	if err != nil {
		return Token{}, err
	}
	return tok, nil
}

func revokeInTx(ctx context.Context, tx pgx.Tx, id, by string, a Audit) (Token, error) {
	var live bool
	err := tx.QueryRow(ctx, `/* pg_sage mcp_token_revoke v2 */
		SELECT revoked_at IS NULL FROM sage.mcp_tokens WHERE id = $1::uuid FOR UPDATE`,
		id).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("mcptoken: locking token %s: %w", id, err)
	}
	tok, err := scanToken(tx.QueryRow(ctx, `/* pg_sage mcp_token_revoke v2 */
		UPDATE sage.mcp_tokens
		SET revoked_at = COALESCE(revoked_at, now()),
			revoked_by = CASE WHEN revoked_at IS NULL THEN $2 ELSE revoked_by END
		WHERE id = $1::uuid
		RETURNING `+tokenColumns, id, by))
	if err != nil {
		return Token{}, fmt.Errorf("mcptoken: revoking token %s: %w", id, err)
	}
	if !live {
		return tok, nil
	}
	return tok, recordRevoked(ctx, tx, a, tok)
}
