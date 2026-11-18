package pressure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
)

// Backend is one session of a registered job that is writing: an active
// client backend of this database, under the job's application_name,
// whose transaction holds a transaction ID (it has written). It pins the
// statement it runs (query_start and a hash of its text) for the
// executor's identity recheck; the text itself is never kept.
type Backend struct {
	PID             int32     `json:"pid"`
	BackendStart    time.Time `json:"backend_start"`
	QueryStart      time.Time `json:"query_start"`
	Database        string    `json:"database"`
	User            string    `json:"user"`
	ApplicationName string    `json:"application_name"`
	QueryHash       string    `json:"query_hash"`
	QueryID         int64     `json:"query_id,string"`
	ObservedAt      time.Time `json:"observed_at"`
}

// Identity is the executor's view of the backend's statement.
func (b Backend) Identity() executor.BackendIdentity {
	return executor.BackendIdentity{PID: b.PID, BackendStart: b.BackendStart,
		QueryStart: b.QueryStart, Database: b.Database, User: b.User,
		QueryHash: b.QueryHash, QueryID: b.QueryID}
}

// BackendFinder reads a job's writing sessions.
type BackendFinder interface {
	// Writing lists the job's writing sessions, oldest first.
	Writing(ctx context.Context, applicationName string) ([]Backend, error)
	// Recheck reads one incarnation (pid and backend_start) again; false
	// when it no longer runs under the name or no longer writes.
	Recheck(ctx context.Context, applicationName string, pid int32,
		backendStart time.Time) (Backend, bool, error)
}

// SQLBackends reads pg_stat_activity in a bounded read-only transaction.
type SQLBackends struct {
	src *SQLSource
}

// NewSQLBackends reads pool's database.
func NewSQLBackends(pool *pgxpool.Pool) *SQLBackends {
	return &SQLBackends{src: NewSQLSource(pool, SQLOptions{})}
}

// maxListedBackends bounds one Writing read.
const maxListedBackends = 32

const writingSQL = `/* pg_sage pressure */
SELECT a.pid, a.backend_start, a.query_start, a.datname, a.usename, a.application_name,
       pg_catalog.encode(pg_catalog.sha256(convert_to(a.query, 'UTF8')), 'hex'),
       COALESCE(a.query_id, 0)
  FROM pg_catalog.pg_stat_activity AS a
 WHERE a.datname = pg_catalog.current_database()
   AND a.application_name = $1
   AND a.backend_type = 'client backend'
   AND a.state = 'active'
   AND a.backend_xid IS NOT NULL
   AND a.query_start IS NOT NULL
   AND a.pid <> pg_catalog.pg_backend_pid()
   AND ($2::int IS NULL OR (a.pid = $2 AND a.backend_start = $3))
 ORDER BY a.backend_start, a.pid
 LIMIT $4`

// Writing lists the job's writing sessions.
func (b *SQLBackends) Writing(ctx context.Context, applicationName string) ([]Backend,
	error) {
	return b.read(ctx, applicationName, nil, time.Time{})
}

// Recheck reads one incarnation again.
func (b *SQLBackends) Recheck(ctx context.Context, applicationName string, pid int32,
	backendStart time.Time) (Backend, bool, error) {
	out, err := b.read(ctx, applicationName, &pid, backendStart)
	if err != nil || len(out) == 0 {
		return Backend{}, false, err
	}
	return out[0], true, nil
}

func (b *SQLBackends) read(ctx context.Context, app string, pid *int32,
	start time.Time) ([]Backend, error) {
	if b == nil || b.src == nil || b.src.pool == nil {
		return nil, ErrUnavailable
	}
	var startArg any
	if pid != nil {
		startArg = start
	}
	var out []Backend
	err := b.src.readTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, writingSQL, app, pid, startArg, maxListedBackends)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, scanBackend)
		return err
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("reading the sessions of %q from pg_stat_activity: %w", app, err)
	}
	now := time.Now()
	for i := range out {
		out[i].ObservedAt = now
	}
	return out, nil
}

func scanBackend(r pgx.CollectableRow) (Backend, error) {
	var b Backend
	err := r.Scan(&b.PID, &b.BackendStart, &b.QueryStart, &b.Database, &b.User,
		&b.ApplicationName, &b.QueryHash, &b.QueryID)
	return b, err
}
