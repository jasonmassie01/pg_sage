package pressure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Job registration (guard-01b): a backfill registers the application_name
// its sessions run under, so pg_sage can tie a backend to the job. A job
// with Enforce set lets pg_sage propose cancelling the job's backend when
// the job keeps writing through two consecutive stop verdicts; the
// proposal always waits for a person's approval (enforce.go).

const (
	// DefaultJobTTL is a registration's life when it names none.
	DefaultJobTTL = 24 * time.Hour
	// MaxJobTTL bounds a registration; a longer job registers again.
	MaxJobTTL = 7 * 24 * time.Hour
	// maxApplicationName is PostgreSQL's application_name limit
	// (NAMEDATALEN - 1); a longer name is truncated by the server.
	maxApplicationName = 63
	maxRegisteredBy    = 200
	maxDatabaseName    = 200
)

var (
	// ErrJobNotFound is a job id the database does not have.
	ErrJobNotFound = errors.New("pressure job not found")
	// ErrJobExpired is a job whose registration ended.
	ErrJobExpired = errors.New("pressure job expired")
)

// JobSpec is one registration request.
type JobSpec struct {
	Database        string
	ApplicationName string
	// Table is the table the job writes ("" for none): the enforcer's
	// checks name it, so its table inputs count.
	Table        string
	RegisteredBy string
	Enforce      bool
	// TTL is how long the registration lasts (0: DefaultJobTTL).
	TTL time.Duration
}

// Job is one registered job.
type Job struct {
	ID              int64     `json:"id"`
	Database        string    `json:"database"`
	ApplicationName string    `json:"application_name"`
	Table           string    `json:"table_name,omitempty"`
	RegisteredBy    string    `json:"registered_by"`
	Enforce         bool      `json:"enforce"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// Active reports whether the job's registration lasts past now.
func (j Job) Active(now time.Time) bool { return j.ExpiresAt.After(now) }

// JobRegistry is what the API needs of the job store.
type JobRegistry interface {
	Register(ctx context.Context, spec JobSpec) (Job, error)
	Active(ctx context.Context, database string) ([]Job, error)
	End(ctx context.Context, database string, id int64) (Job, error)
}

func (s JobSpec) ttl() time.Duration {
	if s.TTL == 0 {
		return DefaultJobTTL
	}
	return s.TTL
}

// Validate refuses a spec the store would not keep.
func (s JobSpec) Validate() error {
	name := s.ApplicationName
	switch {
	case strings.TrimSpace(s.Database) == "" || len(s.Database) > maxDatabaseName:
		return fmt.Errorf("%w: database name must be 1-%d bytes", ErrInvalidRequest,
			maxDatabaseName)
	case name == "" || len(name) > maxApplicationName || !printableASCII(name):
		return fmt.Errorf("%w: application_name must be 1-%d printable ASCII bytes, as "+
			"PostgreSQL keeps it", ErrInvalidRequest, maxApplicationName)
	case strings.Contains(strings.ToLower(name), "pg_sage"):
		return fmt.Errorf("%w: application_name %q names pg_sage's own sessions, which are "+
			"never signalled", ErrInvalidRequest, name)
	case strings.TrimSpace(s.RegisteredBy) == "" || len(s.RegisteredBy) > maxRegisteredBy:
		return fmt.Errorf("%w: a registration names who made it", ErrInvalidRequest)
	case s.TTL < 0 || s.TTL > MaxJobTTL:
		return fmt.Errorf("%w: ttl %s outside (0, %s]", ErrInvalidRequest, s.TTL, MaxJobTTL)
	case len(s.Table) > maxTableName || strings.ContainsRune(s.Table, 0):
		return fmt.Errorf("%w: table name over %d bytes", ErrInvalidRequest, maxTableName)
	}
	return nil
}

func printableASCII(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// JobStore keeps registered jobs in the monitored database's sage schema.
type JobStore struct {
	pool *pgxpool.Pool
}

// NewJobStore binds the store to a monitored database's pool.
func NewJobStore(pool *pgxpool.Pool) *JobStore { return &JobStore{pool: pool} }

const jobColumns = `id, database, application_name, COALESCE(table_name, ''),
	registered_by, enforce, created_at, expires_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Database, &j.ApplicationName, &j.Table, &j.RegisteredBy,
		&j.Enforce, &j.CreatedAt, &j.ExpiresAt)
	return j, err
}

// Register records spec. A registration while the same database and name
// has an active job updates that job (table, enforce, registrant, expiry)
// instead of adding a second one; concurrent registrations serialize on an
// advisory lock of the pair.
func (s *JobStore) Register(ctx context.Context, spec JobSpec) (Job, error) {
	if err := spec.Validate(); err != nil {
		return Job{}, err
	}
	var j Job
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(
			hashtextextended('pressure_job:' || $1 || ':' || $2, 0))`, spec.Database,
			spec.ApplicationName); err != nil {
			return err
		}
		var err error
		j, err = scanJob(tx.QueryRow(ctx, `/* pg_sage */ UPDATE sage.pressure_jobs
			SET table_name = NULLIF($3, ''), registered_by = $4, enforce = $5,
			    expires_at = now() + make_interval(secs => $6)
			WHERE database = $1 AND application_name = $2 AND expires_at > now()
			RETURNING `+jobColumns, spec.Database, spec.ApplicationName, spec.Table,
			spec.RegisteredBy, spec.Enforce, spec.ttl().Seconds()))
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		j, err = scanJob(tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.pressure_jobs
			(database, application_name, table_name, registered_by, enforce, expires_at)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, now() + make_interval(secs => $6))
			RETURNING `+jobColumns, spec.Database, spec.ApplicationName, spec.Table,
			spec.RegisteredBy, spec.Enforce, spec.ttl().Seconds()))
		return err
	})
	if err != nil {
		return Job{}, fmt.Errorf("registering pressure job %q in %s: %w",
			spec.ApplicationName, spec.Database, err)
	}
	return j, nil
}

// Active lists database's jobs whose registration has not ended.
func (s *JobStore) Active(ctx context.Context, database string) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+jobColumns+`
		FROM sage.pressure_jobs WHERE database = $1 AND expires_at > now()
		ORDER BY id LIMIT 1000`, database)
	if err != nil {
		return nil, fmt.Errorf("listing pressure jobs of %s: %w", database, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) {
		return scanJob(r)
	})
	if err != nil {
		return nil, fmt.Errorf("reading pressure jobs of %s: %w", database, err)
	}
	return out, nil
}

// Get reads one job, active or not.
func (s *JobStore) Get(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+jobColumns+`
		FROM sage.pressure_jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, fmt.Errorf("%w: id %d", ErrJobNotFound, id)
	}
	if err != nil {
		return Job{}, fmt.Errorf("reading pressure job %d: %w", id, err)
	}
	return j, nil
}

// End ends database's active job id now (a finished backfill unregisters).
func (s *JobStore) End(ctx context.Context, database string, id int64) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `/* pg_sage */ UPDATE sage.pressure_jobs
		SET expires_at = GREATEST(now(), created_at + interval '1 microsecond')
		WHERE id = $1 AND database = $2 AND expires_at > now()
		RETURNING `+jobColumns, id, database))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, fmt.Errorf("%w: no active job %d in %s", ErrJobNotFound, id, database)
	}
	if err != nil {
		return Job{}, fmt.Errorf("ending pressure job %d: %w", id, err)
	}
	return j, nil
}
