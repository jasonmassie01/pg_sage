package rubric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind is what a run is: a grade alone, or the grade of a review.
type Kind string

// Run kinds.
const (
	KindGrade  Kind = "grade"
	KindReview Kind = "review"
)

// RunStatus is a run's state: running moves once, to done, partial (some
// producer could not be read) or failed, and then never moves again.
type RunStatus string

// Run statuses.
const (
	RunRunning RunStatus = "running"
	RunDone    RunStatus = "done"
	RunPartial RunStatus = "partial"
	RunFailed  RunStatus = "failed"
)

// ReapedError is the error of a run a killed sidecar left running.
const ReapedError = "sidecar stopped during run"

// Errors, each distinguishable with errors.Is.
var (
	ErrNoStore    = errors.New("review: no database connection pool")
	ErrRunning    = errors.New("review: a run is already running for this database")
	ErrNotRunning = errors.New("review: the run is not running")
	ErrInvalidRun = errors.New("review: invalid run")
)

const runningIndex = "review_run_one_running"

// RunSpec is a run to start.
type RunSpec struct {
	Database      string
	Kind          Kind
	Triggers      []string
	RubricVersion string
}

// Run is one stored run with its grade (empty unless done or partial).
type Run struct {
	ID            int64      `json:"id"`
	Database      string     `json:"database"`
	Kind          Kind       `json:"kind"`
	Triggers      []string   `json:"triggers"`
	RubricVersion string     `json:"rubric_version"`
	Status        RunStatus  `json:"status"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	Error         string     `json:"error,omitempty"`
	Grade         Grade      `json:"grade"`
}

// Store keeps runs in sage.review_run and their dimensions in
// sage.review_grade, in the graded database's own sage schema.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) usable() error {
	if s == nil || s.pool == nil {
		return ErrNoStore
	}
	return nil
}

// Start records a running run. A second run of one database while one is
// running is ErrRunning (the partial unique index decides, so two
// sidecars racing get one row).
func (s *Store) Start(ctx context.Context, spec RunSpec) (int64, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	if spec.Database == "" || spec.RubricVersion == "" ||
		(spec.Kind != KindGrade && spec.Kind != KindReview) {
		return 0, fmt.Errorf("%w: database %q, kind %q, rubric %q", ErrInvalidRun,
			spec.Database, spec.Kind, spec.RubricVersion)
	}
	triggers := spec.Triggers
	if triggers == nil {
		triggers = []string{}
	}
	var id int64
	err := s.pool.QueryRow(ctx, `/* pg_sage review v1 */
		INSERT INTO sage.review_run (database_name, kind, triggers, rubric_version)
		VALUES ($1, $2, $3, $4) RETURNING id`, spec.Database, string(spec.Kind), triggers,
		spec.RubricVersion).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == runningIndex {
		return 0, fmt.Errorf("%w: %s", ErrRunning, spec.Database)
	}
	if err != nil {
		return 0, fmt.Errorf("insert into sage.review_run for %s: %w", spec.Database, err)
	}
	return id, nil
}

// Finish moves a running run to status with its grade (nil for a failed
// run) in one transaction. A run that is not running is ErrNotRunning.
func (s *Store) Finish(ctx context.Context, id int64, status RunStatus, g *Grade,
	runErr string) error {
	if err := s.usable(); err != nil {
		return err
	}
	if status != RunDone && status != RunPartial && status != RunFailed {
		return fmt.Errorf("%w: cannot finish run %d as %q", ErrInvalidRun, id, status)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := finishRow(ctx, tx, id, status, g, runErr); err != nil {
			return err
		}
		if g == nil {
			return nil
		}
		return insertDimensions(ctx, tx, id, g.Dimensions)
	})
}

func finishRow(ctx context.Context, tx pgx.Tx, id int64, status RunStatus, g *Grade,
	runErr string) error {
	var letter, score, coverage any
	if g != nil {
		score, coverage = g.OverallScore, g.CoveragePct
		if g.Overall != "" {
			letter = string(g.Overall)
		}
	}
	tag, err := tx.Exec(ctx, `/* pg_sage review v1 */
		UPDATE sage.review_run SET status = $2, overall_grade = $3, overall_score = $4,
		       coverage_pct = $5, error = nullif($6, ''), finished_at = now()
		WHERE id = $1 AND status = 'running'`, id, string(status), letter, score, coverage,
		runErr)
	if err != nil {
		return fmt.Errorf("update sage.review_run %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %d", ErrNotRunning, id)
	}
	return nil
}

func insertDimensions(ctx context.Context, tx pgx.Tx, id int64, dims []DimensionGrade) error {
	for i, d := range dims {
		checks, err := json.Marshal(d.Checks)
		if err != nil {
			return fmt.Errorf("encode the checks of %s: %w", d.Dimension, err)
		}
		var letter any
		if d.Letter != "" {
			letter = string(d.Letter)
		}
		if _, err := tx.Exec(ctx, `/* pg_sage review v1 */
			INSERT INTO sage.review_grade (run_id, ordinal, dimension, title, letter, score,
			    coverage, checks) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, i, d.Dimension,
			d.Title, letter, d.Score, d.Coverage, checks); err != nil {
			return fmt.Errorf("insert into sage.review_grade for run %d: %w", id, err)
		}
	}
	return nil
}

// Reap fails database's runs left running for longer than staleAfter: a
// sidecar killed during a run never finishes it.
func (s *Store) Reap(ctx context.Context, database string, staleAfter time.Duration) (
	int64, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, `/* pg_sage review v1 */
		UPDATE sage.review_run SET status = 'failed', error = $3, finished_at = now()
		WHERE database_name = $1 AND status = 'running'
		  AND started_at < now() - make_interval(secs => $2)`, database,
		staleAfter.Seconds(), ReapedError)
	if err != nil {
		return 0, fmt.Errorf("reap sage.review_run of %s: %w", database, err)
	}
	return tag.RowsAffected(), nil
}
