package recommendation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// candidateSQL selects heads joined to their current revision.
const candidateSQL = `/* pg_sage */ SELECT ` + headColumns + `, ` + revisionColumns + `
	FROM sage.recommendation r
	JOIN sage.recommendation_revision v
	  ON v.recommendation_id = r.id AND v.revision = r.revision `

func scanCandidate(row pgx.Row) (Candidate, error) {
	var c Candidate
	var state string
	var rev revisionRow
	if err := row.Scan(append(headDest(&c.Recommendation, &state), rev.dest()...)...); err != nil {
		return c, err
	}
	c.State = State(state)
	current, err := rev.decode()
	c.Current = current
	return c, err
}

// ListActionable returns the heads of database the executor may act on:
// proposed and approved rows, and failed rows whose backoff has passed.
func (s *Store) ListActionable(ctx context.Context, database string) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, candidateSQL+`
		WHERE r.database_name = $1
		  AND (r.state IN ('proposed', 'approved')
		       OR (r.state = 'failed' AND r.next_attempt_at <= now()))
		ORDER BY r.id`, database)
	if err != nil {
		return nil, fmt.Errorf("list actionable recommendations: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Candidate, error) {
		return scanCandidate(row)
	})
}

// FindForOperator returns the live recommendation of a finding whose
// current revision runs forwardSQL, or nil when there is none.
func (s *Store) FindForOperator(
	ctx context.Context, findingID int64, forwardSQL string,
) (*Candidate, error) {
	c, err := scanCandidate(s.pool.QueryRow(ctx, candidateSQL+`
		WHERE r.finding_id = $1 AND v.forward_sql = $2
		  AND r.state IN `+revisableStatesSQL+`
		ORDER BY r.id DESC LIMIT 1`, findingID, strings.TrimSpace(forwardSQL)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find recommendation for finding %d: %w", findingID, err)
	}
	return &c, nil
}

// CheckFresh re-checks a candidate before acting (C07): the head must be
// unchanged since it was read, and its finding must still be open. A
// closed finding means the recommendation should be superseded.
func (s *Store) CheckFresh(ctx context.Context, c Candidate) (Freshness, error) {
	var state, findingStatus string
	var revision int
	err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT r.state, r.revision,
		COALESCE(f.status, '')
		FROM sage.recommendation r
		LEFT JOIN sage.findings f ON f.id = r.finding_id
		WHERE r.id = $1`, c.ID).Scan(&state, &revision, &findingStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Freshness{Reason: "recommendation no longer exists"}, nil
	}
	if err != nil {
		return Freshness{}, fmt.Errorf("check recommendation %d: %w", c.ID, err)
	}
	if State(state) != c.State || revision != c.Revision {
		return Freshness{Reason: fmt.Sprintf("changed since read: now %s at revision %d",
			state, revision)}, nil
	}
	if findingStatus != "open" {
		return Freshness{Supersede: true,
			Reason: "its finding is no longer open"}, nil
	}
	return Freshness{Fresh: true}, nil
}

// SupersedeAbsent supersedes the not-in-flight recommendations of one
// evaluated category that the analyzer no longer emits (active holds the
// identity keys it did emit). In-flight and terminal rows are untouched.
func (s *Store) SupersedeAbsent(
	ctx context.Context, database, category string, active map[string]bool,
) (int, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, state, revision, identity_key
		FROM sage.recommendation
		WHERE database_name = $1 AND category = $2 AND state IN `+revisableStatesSQL,
		database, category)
	if err != nil {
		return 0, fmt.Errorf("list recommendations of %s: %w", category, err)
	}
	type live struct {
		m   move
		key string
	}
	heads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (live, error) {
		var l live
		var state string
		err := row.Scan(&l.m.id, &state, &l.m.revision, &l.key)
		l.m.from = State(state)
		return l, err
	})
	if err != nil {
		return 0, err
	}
	superseded := 0
	for _, h := range heads {
		if active[h.key] {
			continue
		}
		err := s.Supersede(ctx, h.m.id, h.m.from, h.m.revision, ActorAnalyzer,
			"no longer recommended by the analyzer")
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return superseded, err
		}
		superseded++
	}
	return superseded, nil
}

// Supersede ends a not-in-flight recommendation (compare-and-set on its
// state and revision), recording why.
func (s *Store) Supersede(
	ctx context.Context, id int64, from State, revision int, actor, reason string,
) error {
	m := move{id: id, from: from, to: StateSuperseded, revision: revision, actor: actor,
		reason: reason}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		return applyMove(ctx, tx, m, `, reason = $6, next_attempt_at = NULL`, reason)
	})
}
