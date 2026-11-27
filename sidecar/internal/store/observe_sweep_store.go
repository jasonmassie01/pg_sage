package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Observe-only sweeps (fleetpol-01d): each observe-only member's newest
// sweep, and the rounds the fleet sweeper ran. Both live in the meta
// database; nothing is written to a swept member.

// Sweep statuses.
const (
	ObserveOK          = "ok"          // the checks ran and the member is graded
	ObserveUnreachable = "unreachable" // no connection could be had
	ObserveFailed      = "failed"      // connected, but the checks failed
)

// observeRoundRetention is how long finished rounds are kept.
const observeRoundRetention = 30 * 24 * time.Hour

// ObserveSweep is one member's newest sweep.
type ObserveSweep struct {
	DatabaseID int
	RoundID    int64
	SweptAt    time.Time
	Status     string
	Error      string
	Letter     string
	Grade      json.RawMessage // the rubric grade; nil or empty is {}
	DurationMS int64
}

// ObserveRound is one sweep round over every observe-only member.
type ObserveRound struct {
	ID            int64
	StartedAt     time.Time
	FinishedAt    time.Time
	Members       int
	OK            int
	Unreachable   int
	Failed        int
	Promoted      int
	MaxConcurrent int
}

// BeginObserveRound records a round starting with members to visit.
func (s *DatabaseStore) BeginObserveRound(ctx context.Context, started time.Time,
	members int) (int64, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id int64
	if err := s.pool.QueryRow(qctx, `/* pg_sage */ INSERT INTO sage.observe_rounds
		(started_at, members) VALUES ($1, $2) RETURNING id`, started, members).Scan(
		&id); err != nil {
		return 0, fmt.Errorf("recording an observe-only round: %w", err)
	}
	return id, nil
}

// RecordObserveSweep keeps sw as its member's newest sweep.
func (s *DatabaseStore) RecordObserveSweep(ctx context.Context, sw ObserveSweep) error {
	switch sw.Status {
	case ObserveOK, ObserveUnreachable, ObserveFailed:
	default:
		return fmt.Errorf("%w: sweep status %q must be ok, unreachable or failed",
			ErrValidation, sw.Status)
	}
	grade := sw.Grade
	if len(grade) == 0 {
		grade = json.RawMessage(`{}`)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(qctx, `/* pg_sage */ INSERT INTO sage.observe_sweeps
		(database_id, round_id, swept_at, status, error, letter, grade, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (database_id) DO UPDATE SET round_id = EXCLUDED.round_id,
		    swept_at = EXCLUDED.swept_at, status = EXCLUDED.status,
		    error = EXCLUDED.error, letter = EXCLUDED.letter, grade = EXCLUDED.grade,
		    duration_ms = EXCLUDED.duration_ms`,
		sw.DatabaseID, sw.RoundID, sw.SweptAt, sw.Status, sw.Error, sw.Letter,
		[]byte(grade), max(sw.DurationMS, 0))
	if err != nil {
		return fmt.Errorf("recording the sweep of database %d: %w", sw.DatabaseID, err)
	}
	return nil
}

// FinishObserveRound records a round's end and counts, and forgets rounds
// older than 30 days.
func (s *DatabaseStore) FinishObserveRound(ctx context.Context, r ObserveRound) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.observe_rounds
		SET finished_at = $2, members = $3, ok = $4, unreachable = $5, failed = $6,
		    promoted = $7, max_concurrent = $8
		WHERE id = $1`, r.ID, r.FinishedAt, r.Members, r.OK, r.Unreachable, r.Failed,
		r.Promoted, r.MaxConcurrent)
	if err != nil {
		return fmt.Errorf("finishing observe-only round %d: %w", r.ID, err)
	}
	if _, err := s.pool.Exec(qctx, `/* pg_sage */ DELETE FROM sage.observe_rounds
		WHERE started_at < $1`, r.StartedAt.Add(-observeRoundRetention)); err != nil {
		return fmt.Errorf("pruning observe-only rounds: %w", err)
	}
	return nil
}

// ObserveSweeps are every member's newest sweep, by member.
func (s *DatabaseStore) ObserveSweeps(ctx context.Context) ([]ObserveSweep, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT database_id, round_id,
		swept_at, status, error, letter, grade, duration_ms
		FROM sage.observe_sweeps ORDER BY database_id`)
	if err != nil {
		return nil, fmt.Errorf("reading observe-only sweeps: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ObserveSweep, error) {
		var sw ObserveSweep
		var grade []byte
		err := row.Scan(&sw.DatabaseID, &sw.RoundID, &sw.SweptAt, &sw.Status, &sw.Error,
			&sw.Letter, &grade, &sw.DurationMS)
		sw.Grade = grade
		return sw, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading observe-only sweeps: %w", err)
	}
	return out, nil
}

// LatestObserveRound is the newest round; ok is false when none ran.
func (s *DatabaseStore) LatestObserveRound(ctx context.Context) (ObserveRound, bool,
	error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var r ObserveRound
	var finished *time.Time
	err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT id, started_at, finished_at,
		members, ok, unreachable, failed, promoted, max_concurrent
		FROM sage.observe_rounds ORDER BY id DESC LIMIT 1`).Scan(&r.ID, &r.StartedAt,
		&finished, &r.Members, &r.OK, &r.Unreachable, &r.Failed, &r.Promoted,
		&r.MaxConcurrent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ObserveRound{}, false, nil
	}
	if err != nil {
		return ObserveRound{}, false, fmt.Errorf("reading the newest observe-only round: %w",
			err)
	}
	if finished != nil {
		r.FinishedAt = *finished
	}
	return r, true, nil
}

// ObserveSweepByName is the newest sweep of the observe-only member name:
// observe is false when no observe-only member has that name, and sweep is
// nil when it has not been swept yet.
func (s *DatabaseStore) ObserveSweepByName(ctx context.Context, name string) (
	sweep *ObserveSweep, observe bool, err error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id int
	var round *int64
	var at *time.Time
	var status, errText, letter *string
	var grade []byte
	var ms *int64
	err = s.pool.QueryRow(qctx, `/* pg_sage */ SELECT m.id, s.round_id, s.swept_at,
		s.status, s.error, s.letter, s.grade, s.duration_ms
		FROM sage.databases m LEFT JOIN sage.observe_sweeps s ON s.database_id = m.id
		WHERE m.name = $1 AND m.tier = 'observe_only'`, name).Scan(&id, &round, &at,
		&status, &errText, &letter, &grade, &ms)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the sweep of %q: %w", name, err)
	}
	if round == nil {
		return nil, true, nil
	}
	return &ObserveSweep{DatabaseID: id, RoundID: *round, SweptAt: *at, Status: *status,
		Error: *errText, Letter: *letter, Grade: grade, DurationMS: *ms}, true, nil
}
