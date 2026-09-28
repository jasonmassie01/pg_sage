package recommendation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// headColumns are the columns scanHead reads, in order, from the head
// aliased r.
const headColumns = `r.id, r.identity_key, r.database_name, r.category, r.target,
	r.action_type, r.index_fingerprint, r.finding_id, r.state, r.revision, r.content_hash,
	r.approved_revision, COALESCE(r.approved_hash, ''), COALESCE(r.approved_by, ''),
	r.approved_at, r.attempt_count, r.retry_budget, r.next_attempt_at, r.lease_until,
	r.action_log_id, COALESCE(r.verdict, ''), COALESCE(r.reason, ''), r.created_at,
	r.updated_at, r.last_seen_at, COALESCE(r.next_attempt_at <= now(), true)`

// headDest lists the scan destinations of headColumns.
func headDest(r *Recommendation, state *string) []any {
	return []any{&r.ID, &r.IdentityKey, &r.DatabaseName, &r.Category, &r.Target,
		&r.ActionType, &r.IndexFingerprint, &r.FindingID, state, &r.Revision,
		&r.ContentHash, &r.ApprovedRevision, &r.ApprovedHash, &r.ApprovedBy,
		&r.ApprovedAt, &r.AttemptCount, &r.RetryBudget, &r.NextAttemptAt, &r.LeaseUntil,
		&r.ActionLogID, &r.Verdict, &r.Reason, &r.CreatedAt, &r.UpdatedAt,
		&r.LastSeenAt, &r.due}
}

func scanHead(row pgx.Row) (Recommendation, error) {
	var r Recommendation
	var state string
	err := row.Scan(headDest(&r, &state)...)
	r.State = State(state)
	return r, err
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getTx(ctx context.Context, q querier, id int64) (Recommendation, error) {
	rec, err := scanHead(q.QueryRow(ctx, `/* pg_sage */ SELECT `+headColumns+`
		FROM sage.recommendation r WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Recommendation{}, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	return rec, err
}

// Get returns one head.
func (s *Store) Get(ctx context.Context, id int64) (Recommendation, error) {
	return getTx(ctx, s.pool, id)
}

// maxListLimit bounds one List page.
const maxListLimit = 500

// List returns heads of one database, newest first.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Recommendation, error) {
	if f.State != "" && !f.State.Valid() {
		return nil, fmt.Errorf("unknown recommendation state %q", f.State)
	}
	limit := f.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+headColumns+`
		FROM sage.recommendation r
		WHERE r.database_name = $1 AND ($2 = '' OR r.state = $2)
		ORDER BY r.id DESC LIMIT $3`, f.DatabaseName, string(f.State), limit)
	if err != nil {
		return nil, fmt.Errorf("list recommendations: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recommendation, error) {
		return scanHead(row)
	})
}

const revisionColumns = `v.recommendation_id, v.revision, v.content_hash, v.forward_sql,
	v.inverse_sql, v.evidence, v.preconditions, v.policy_version, v.title, v.severity,
	v.object_type, v.action_risk, v.recommendation, v.source, v.created_at`

// revisionRow holds a scanned revision before its JSON is decoded.
type revisionRow struct {
	v             Revision
	evidence, pre []byte
}

func (r *revisionRow) dest() []any {
	v := &r.v
	return []any{&v.RecommendationID, &v.Revision, &v.ContentHash, &v.ForwardSQL,
		&v.InverseSQL, &r.evidence, &r.pre, &v.PolicyVersion, &v.Title, &v.Severity,
		&v.ObjectType, &v.ActionRisk, &v.Recommendation, &v.Source, &v.CreatedAt}
}

func (r *revisionRow) decode() (Revision, error) {
	if err := json.Unmarshal(r.evidence, &r.v.Evidence); err != nil {
		return r.v, fmt.Errorf("decode revision evidence: %w", err)
	}
	if err := json.Unmarshal(r.pre, &r.v.Preconditions); err != nil {
		return r.v, fmt.Errorf("decode revision preconditions: %w", err)
	}
	return r.v, nil
}

func scanRevision(row pgx.Row) (Revision, error) {
	var r revisionRow
	if err := row.Scan(r.dest()...); err != nil {
		return r.v, err
	}
	return r.decode()
}

// Revisions returns every revision of id, oldest first.
func (s *Store) Revisions(ctx context.Context, id int64) ([]Revision, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+revisionColumns+`
		FROM sage.recommendation_revision v
		WHERE v.recommendation_id = $1 ORDER BY v.revision`, id)
	if err != nil {
		return nil, fmt.Errorf("list recommendation revisions: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Revision, error) {
		return scanRevision(row)
	})
}

// Transitions returns the history of id, oldest first.
func (s *Store) Transitions(ctx context.Context, id int64) ([]Transition, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, recommendation_id,
		COALESCE(from_state, ''), to_state, revision, actor, reason, action_log_id,
		created_at
		FROM sage.recommendation_transition
		WHERE recommendation_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("list recommendation transitions: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Transition, error) {
		var t Transition
		var from, to string
		err := row.Scan(&t.ID, &t.RecommendationID, &from, &to, &t.Revision, &t.Actor,
			&t.Reason, &t.ActionLogID, &t.CreatedAt)
		t.From, t.To = State(from), State(to)
		return t, err
	})
}
