package rubric

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// maxLatest bounds Latest.
const maxLatest = 50

// gradedRunsSQL reads database $1's graded runs (done or partial), at
// most $2, in id order: newest first, or oldest first.
const gradedRunsSQL = `/* pg_sage review v1 */
		SELECT id, database_name, kind, triggers, rubric_version, status, started_at,
		       finished_at, coalesce(error, ''), overall_grade,
		       coalesce(overall_score, 0)::float8, coalesce(coverage_pct, 0)::float8
		FROM sage.review_run
		WHERE database_name = $1 AND status IN ('done', 'partial')
		ORDER BY `

// Latest is database's newest n graded runs (done or partial), newest
// first, each with its dimensions.
func (s *Store) Latest(ctx context.Context, database string, n int) ([]Run, error) {
	return s.graded(ctx, database, n, gradedRunsSQL+"id DESC LIMIT $2")
}

// Oldest is database's first n graded runs, oldest first, each with its
// dimensions: fleet discovery dates a member's first grade and the first
// that meets its policy set's baseline from them (fleetpol-01b).
func (s *Store) Oldest(ctx context.Context, database string, n int) ([]Run, error) {
	return s.graded(ctx, database, n, gradedRunsSQL+"id ASC LIMIT $2")
}

func (s *Store) graded(ctx context.Context, database string, n int, sql string) (
	[]Run, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	n = min(max(n, 1), maxLatest)
	rows, err := s.pool.Query(ctx, sql, database, n)
	if err != nil {
		return nil, fmt.Errorf("read sage.review_run of %s: %w", database, err)
	}
	runs, err := pgx.CollectRows(rows, scanRun)
	if err != nil {
		return nil, fmt.Errorf("read sage.review_run of %s: %w", database, err)
	}
	for i := range runs {
		if err := s.loadDimensions(ctx, &runs[i]); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func scanRun(row pgx.CollectableRow) (Run, error) {
	var r Run
	var kind, status string
	var overall *string
	err := row.Scan(&r.ID, &r.Database, &kind, &r.Triggers, &r.RubricVersion, &status,
		&r.StartedAt, &r.FinishedAt, &r.Error, &overall, &r.Grade.OverallScore,
		&r.Grade.CoveragePct)
	r.Kind, r.Status, r.Grade.RubricVersion = Kind(kind), RunStatus(status), r.RubricVersion
	if overall != nil {
		r.Grade.Overall = Letter(*overall)
	}
	return r, err
}

func (s *Store) loadDimensions(ctx context.Context, r *Run) error {
	rows, err := s.pool.Query(ctx, `/* pg_sage review v1 */
		SELECT dimension, title, letter, score::float8, coverage::float8, checks
		FROM sage.review_grade WHERE run_id = $1 ORDER BY ordinal`, r.ID)
	if err != nil {
		return fmt.Errorf("read sage.review_grade of run %d: %w", r.ID, err)
	}
	dims, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DimensionGrade, error) {
		var d DimensionGrade
		var letter *string
		var checks []byte
		if err := row.Scan(&d.Dimension, &d.Title, &letter, &d.Score, &d.Coverage,
			&checks); err != nil {
			return d, err
		}
		if letter != nil {
			d.Letter = Letter(*letter)
		}
		return d, json.Unmarshal(checks, &d.Checks)
	})
	if err != nil {
		return fmt.Errorf("read sage.review_grade of run %d: %w", r.ID, err)
	}
	r.Grade.Dimensions = dims
	for _, d := range dims {
		if d.Letter != "" {
			r.Grade.Known++
		}
	}
	return nil
}
