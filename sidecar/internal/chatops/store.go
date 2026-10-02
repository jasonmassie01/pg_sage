package chatops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
)

var (
	// ErrUnmapped means the chat user is not linked to a pg_sage account.
	ErrUnmapped = errors.New("chatops: chat user is not mapped to a pg_sage user")
	// ErrInvalidIdentity means a mapping is malformed or names no account.
	ErrInvalidIdentity = errors.New("chatops: invalid identity mapping")
	// ErrIdentityConflict means the chat user is already mapped.
	ErrIdentityConflict = errors.New("chatops: chat user is already mapped")
	// ErrNotFound means no such mapping.
	ErrNotFound = errors.New("chatops: identity mapping not found")
	// ErrReplay means the callback was already processed.
	ErrReplay = errors.New("chatops: callback already processed")
)

// replayWindow is how long processed callbacks are remembered; it is far
// beyond Slack's signature tolerance and Telegram's redelivery period.
const replayWindow = 24 * time.Hour

// Identity maps one chat user to one pg_sage user.
type Identity struct {
	ID             int       `json:"id"`
	Provider       string    `json:"provider"`
	TeamID         string    `json:"team_id"`
	ExternalUserID string    `json:"external_user_id"`
	UserID         int       `json:"user_id"`
	UserEmail      string    `json:"user_email,omitempty"`
	CreatedBy      int       `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Store keeps chat identity mappings and processed callbacks.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a store on the sidecar's metadata database.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (id Identity) validate() error {
	switch {
	case id.Provider != ProviderSlack && id.Provider != ProviderTelegram:
		return fmt.Errorf("%w: provider must be slack or telegram", ErrInvalidIdentity)
	case strings.TrimSpace(id.ExternalUserID) == "" || len(id.ExternalUserID) > 128:
		return fmt.Errorf("%w: external_user_id must be 1-128 characters",
			ErrInvalidIdentity)
	case id.Provider == ProviderSlack && strings.TrimSpace(id.TeamID) == "":
		return fmt.Errorf("%w: a Slack mapping needs the workspace team_id",
			ErrInvalidIdentity)
	case id.Provider == ProviderTelegram && id.TeamID != "":
		return fmt.Errorf("%w: a Telegram mapping has no team_id", ErrInvalidIdentity)
	case len(id.TeamID) > 64:
		return fmt.Errorf("%w: team_id is too long", ErrInvalidIdentity)
	case id.UserID <= 0:
		return fmt.Errorf("%w: user_id is required", ErrInvalidIdentity)
	}
	return nil
}

// Link maps a chat user to a pg_sage user. A chat user maps to one
// account; relinking requires unlinking first.
func (s *Store) Link(ctx context.Context, id Identity) (Identity, error) {
	if err := id.validate(); err != nil {
		return Identity{}, err
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO sage.chatops_identities
		(provider, team_id, external_user_id, user_id, created_by)
		VALUES ($1, $2, $3, $4, NULLIF($5, 0)) RETURNING id, created_at`,
		id.Provider, id.TeamID, id.ExternalUserID, id.UserID, id.CreatedBy).
		Scan(&id.ID, &id.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return Identity{}, ErrIdentityConflict
		case "23503":
			return Identity{}, fmt.Errorf("%w: no such pg_sage user", ErrInvalidIdentity)
		}
	}
	if err != nil {
		return Identity{}, fmt.Errorf("chatops: link identity: %w", err)
	}
	return id, nil
}

// Unlink removes a mapping.
func (s *Store) Unlink(ctx context.Context, id int) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sage.chatops_identities WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("chatops: unlink identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns every mapping with its account's email.
func (s *Store) List(ctx context.Context) ([]Identity, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.provider, c.team_id,
		c.external_user_id, c.user_id, u.email, COALESCE(c.created_by, 0), c.created_at
		FROM sage.chatops_identities c JOIN sage.users u ON u.id = c.user_id
		ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("chatops: list identities: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Identity, error) {
		var id Identity
		err := r.Scan(&id.ID, &id.Provider, &id.TeamID, &id.ExternalUserID, &id.UserID,
			&id.UserEmail, &id.CreatedBy, &id.CreatedAt)
		return id, err
	})
	if err != nil {
		return nil, fmt.Errorf("chatops: list identities: %w", err)
	}
	return out, nil
}

// Resolve returns the pg_sage account a chat user is mapped to, with its
// current role.
func (s *Store) Resolve(ctx context.Context, provider, teamID,
	externalUserID string) (auth.User, error) {
	var u auth.User
	err := s.pool.QueryRow(ctx, `SELECT u.id, u.email, u.role, u.created_at
		FROM sage.chatops_identities c JOIN sage.users u ON u.id = c.user_id
		WHERE c.provider = $1 AND c.team_id = $2 AND c.external_user_id = $3`,
		provider, teamID, externalUserID).Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, ErrUnmapped
	}
	if err != nil {
		return auth.User{}, fmt.Errorf("chatops: resolve identity: %w", err)
	}
	return u, nil
}

// MarkSeen records a callback delivery; a second delivery of the same
// nonce is ErrReplay. Deliveries older than the replay window are pruned.
func (s *Store) MarkSeen(ctx context.Context, provider, nonce string) error {
	if nonce == "" || len(nonce) > 256 || provider == "" {
		return fmt.Errorf("%w: replay nonce", ErrMalformed)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sage.chatops_replay
		WHERE seen_at < now() - make_interval(secs => $1)`,
		replayWindow.Seconds()); err != nil {
		return fmt.Errorf("chatops: prune replay window: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO sage.chatops_replay (provider, nonce)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, provider, nonce)
	if err != nil {
		return fmt.Errorf("chatops: record callback: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReplay
	}
	return nil
}
