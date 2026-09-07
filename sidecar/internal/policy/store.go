package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Bootstrap(
	ctx context.Context, scope Scope, profile, actor string,
) (Policy, error) {
	current, err := s.Current(ctx, scope)
	if err == nil {
		return current, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Policy{}, err
	}
	document, err := profileDocument(profile)
	if err != nil {
		return Policy{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return Policy{}, fmt.Errorf("bootstrap actor is required")
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO sage.policy (
    database_id, version, scope, profile, doc, status,
    proposed_by, ratified_by, ratified_at, activated_at
) SELECT $1, 1, $2, $3, $4, 'active', $5, $5, now(), now()
WHERE NOT EXISTS (
    SELECT 1 FROM sage.policy
    WHERE database_id IS NOT DISTINCT FROM $1 AND status = 'active'
)`, scope.DatabaseID, scopeName(scope), profile, document, actor)
	if err != nil {
		return Policy{}, fmt.Errorf("bootstrap policy: %w", err)
	}
	return s.Current(ctx, scope)
}

func (s *Store) Current(ctx context.Context, scope Scope) (Policy, error) {
	if s == nil || s.pool == nil {
		return Policy{}, ErrUnavailable
	}
	row := s.pool.QueryRow(ctx, currentPolicySQL, scope.DatabaseID)
	policy, err := scanPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	if err != nil {
		return Policy{}, fmt.Errorf("read current policy: %w", err)
	}
	return policy, nil
}

func (s *Store) Propose(
	ctx context.Context, request ProposalRequest,
) (Proposal, error) {
	if s == nil || s.pool == nil {
		return Proposal{}, ErrUnavailable
	}
	if err := validateProposal(request); err != nil {
		return Proposal{}, err
	}
	current, err := s.Current(ctx, request.Scope)
	if err != nil {
		return Proposal{}, err
	}
	if current.Version != request.ExpectedVersion {
		return Proposal{}, ErrVersionConflict
	}
	preview, err := json.Marshal(request.Preview)
	if err != nil {
		return Proposal{}, fmt.Errorf("encode policy preview: %w", err)
	}
	return s.insertProposal(ctx, request, current, preview)
}

func (s *Store) Ratify(
	ctx context.Context, request RatifyRequest,
) (Policy, error) {
	if s == nil || s.pool == nil {
		return Policy{}, ErrUnavailable
	}
	if request.ProposalID <= 0 || request.ExpectedVersion <= 0 ||
		strings.TrimSpace(request.Actor) == "" {
		return Policy{}, ErrInvalidDocument
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Policy{}, fmt.Errorf("begin policy ratification: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	policy, err := ratifyInTransaction(ctx, tx, request)
	if err != nil {
		return Policy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Policy{}, fmt.Errorf("commit policy ratification: %w", err)
	}
	return policy, nil
}

func (s *Store) History(
	ctx context.Context, scope Scope, limit int,
) ([]Policy, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, policyHistorySQL, scope.DatabaseID, limit)
	if err != nil {
		return nil, fmt.Errorf("read policy history: %w", err)
	}
	defer rows.Close()
	result := make([]Policy, 0)
	for rows.Next() {
		item, scanErr := scanPolicy(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan policy history: %w", scanErr)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy history: %w", err)
	}
	return result, nil
}
