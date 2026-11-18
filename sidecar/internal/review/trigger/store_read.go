package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The reads of sage.review_run and sage.review_trigger_state the sources
// and the scheduler need.

// Review is a finished full review run.
type Review struct {
	ID         int64
	FinishedAt time.Time
}

// LastFullReview is database's newest finished full review (kind review,
// done or partial); ok is false when it has none.
func (s *Store) LastFullReview(ctx context.Context, database string) (Review, bool, error) {
	if err := s.usable(); err != nil {
		return Review{}, false, err
	}
	var r Review
	err := s.pool.QueryRow(ctx, `/* pg_sage review trigger */
		SELECT id, finished_at FROM sage.review_run
		 WHERE database_name = $1 AND kind = 'review' AND status IN ('done', 'partial')
		 ORDER BY finished_at DESC, id DESC LIMIT 1`, database).Scan(&r.ID, &r.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Review{}, false, nil
	}
	if err != nil {
		return Review{}, false, fmt.Errorf("read the last full review of %s: %w",
			database, err)
	}
	r.FinishedAt = r.FinishedAt.UTC()
	return r, true, nil
}

// RunStale is RunStatus's answer for a run still running after staleAfter
// (on the database's clock): its sidecar died mid-run, and the next run's
// reaper fails it.
const RunStale = "stale"

// RunStatus is review run id's status ("" when no such run), or RunStale
// when it has been running for longer than staleAfter (> 0).
func (s *Store) RunStatus(ctx context.Context, id int64, staleAfter time.Duration) (string,
	error) {
	if err := s.usable(); err != nil {
		return "", err
	}
	var st string
	err := s.pool.QueryRow(ctx, `/* pg_sage review trigger */
		SELECT CASE WHEN status = 'running' AND $2::float8 > 0
		             AND started_at < now() - make_interval(secs => $2::float8) THEN 'stale'
		            ELSE status END
		  FROM sage.review_run WHERE id = $1`, id, staleAfter.Seconds()).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read review run %d: %w", id, err)
	}
	return st, nil
}

// LoadFacts is the instance facts last saved for database.
func (s *Store) LoadFacts(ctx context.Context, database string) (Facts, bool, error) {
	if err := s.usable(); err != nil {
		return Facts{}, false, err
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT facts FROM sage.review_trigger_state
		WHERE database_name = $1 AND source = 'instance_change'`, database).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Facts{}, false, nil
	}
	if err != nil {
		return Facts{}, false, fmt.Errorf("read sage.review_trigger_state of %s: %w",
			database, err)
	}
	var f Facts
	if err := json.Unmarshal(raw, &f); err != nil {
		return Facts{}, false, fmt.Errorf("decode the instance facts of %s: %w", database,
			err)
	}
	return f, true, nil
}

// SaveFacts records f as database's last seen instance facts.
func (s *Store) SaveFacts(ctx context.Context, database string, f Facts) error {
	if err := s.usable(); err != nil {
		return err
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode the instance facts of %s: %w", database, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO sage.review_trigger_state
		(database_name, source, facts) VALUES ($1, 'instance_change', $2::jsonb)
		ON CONFLICT (database_name, source)
		DO UPDATE SET facts = EXCLUDED.facts, updated_at = now()`, database,
		string(raw)); err != nil {
		return fmt.Errorf("write sage.review_trigger_state of %s: %w", database, err)
	}
	return nil
}
