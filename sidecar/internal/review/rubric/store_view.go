package rubric

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Read receipt errors (review-01b), each distinguishable with errors.Is.
var (
	ErrRunNotFound = errors.New("review: no such run for this database")
	ErrNotFinished = errors.New("review: the run is not a finished review")
)

// View is a person's first opening of a finished run: First is true only
// on the request that recorded it; later ones return the same ViewedAt.
type View struct {
	RunID    int64     `json:"run_id"`
	ViewedAt time.Time `json:"viewed_at"`
	First    bool      `json:"first"`
}

// MarkViewed records that userID opened run runID of database, once per
// run and user (sage.review_view's primary key decides, so two tabs give
// one row). Only a finished review (done or partial) is recorded: a
// running or failed run is ErrNotFinished, and a run of another database
// or none at all is ErrRunNotFound.
func (s *Store) MarkViewed(ctx context.Context, database string, runID int64,
	userID int) (View, error) {
	if database == "" || runID <= 0 || userID <= 0 {
		return View{}, fmt.Errorf("%w: database %q, run %d, user %d", ErrInvalidRun, database,
			runID, userID)
	}
	if err := s.usable(); err != nil {
		return View{}, err
	}
	if err := s.finishedRun(ctx, database, runID); err != nil {
		return View{}, err
	}
	v := View{RunID: runID, First: true}
	err := s.pool.QueryRow(ctx, `/* pg_sage review v1 */
		INSERT INTO sage.review_view (run_id, user_id) VALUES ($1, $2)
		ON CONFLICT (run_id, user_id) DO NOTHING RETURNING viewed_at`, runID, userID).
		Scan(&v.ViewedAt)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		v.First = false
		at, err := s.ViewedAt(ctx, runID, userID)
		if err != nil {
			return View{}, err
		}
		if at == nil {
			return View{}, fmt.Errorf("no view of run %d by user %d after a conflict",
				runID, userID)
		}
		v.ViewedAt = *at
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return View{}, fmt.Errorf("%w: run %d of %s was deleted", ErrRunNotFound, runID,
			database)
	case err != nil:
		return View{}, fmt.Errorf("insert into sage.review_view for run %d: %w", runID, err)
	}
	v.ViewedAt = v.ViewedAt.UTC()
	return v, nil
}

// finishedRun is nil when runID is a done or partial run of database.
func (s *Store) finishedRun(ctx context.Context, database string, runID int64) error {
	var status string
	err := s.pool.QueryRow(ctx, `/* pg_sage review v1 */
		SELECT status FROM sage.review_run WHERE id = $1 AND database_name = $2`, runID,
		database).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: run %d of %s", ErrRunNotFound, runID, database)
	case err != nil:
		return fmt.Errorf("read sage.review_run %d of %s: %w", runID, database, err)
	case status != string(RunDone) && status != string(RunPartial):
		return fmt.Errorf("%w: run %d of %s is %s", ErrNotFinished, runID, database, status)
	}
	return nil
}

// ViewedAt is when userID first opened run runID, or nil if never.
func (s *Store) ViewedAt(ctx context.Context, runID int64, userID int) (*time.Time, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	var at time.Time
	err := s.pool.QueryRow(ctx, `/* pg_sage review v1 */
		SELECT viewed_at FROM sage.review_view WHERE run_id = $1 AND user_id = $2`, runID,
		userID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sage.review_view of run %d: %w", runID, err)
	}
	at = at.UTC()
	return &at, nil
}

// Running is database's running run (without a grade), or nil.
func (s *Store) Running(ctx context.Context, database string) (*Run, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage review v1 */
		SELECT id, database_name, kind, triggers, rubric_version, status, started_at,
		       finished_at, coalesce(error, ''), overall_grade,
		       coalesce(overall_score, 0)::float8, coalesce(coverage_pct, 0)::float8
		FROM sage.review_run WHERE database_name = $1 AND status = 'running'`, database)
	if err != nil {
		return nil, fmt.Errorf("read the running review of %s: %w", database, err)
	}
	runs, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, fmt.Errorf("read the running review of %s: %w", database, err)
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return &runs[0], nil
}
