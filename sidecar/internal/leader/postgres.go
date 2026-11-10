package leader

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore keeps leases in sage.fleet_leader_lease of the control
// database. Every comparison uses the database clock (now()), so sidecar
// clocks never decide who leads.
type PostgresStore struct {
	pool *pgxpool.Pool

	// noSysid is set once the server rejects pg_control_system() (a
	// managed provider may revoke or hook it): leases are then written
	// with the datname alone, by statements that do not name it.
	noSysid atomic.Bool
}

// NewPostgresStore returns a lease store on pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

var errNoPool = errors.New("no connection pool")

// sysidExpr is the control cluster's system identifier. PostgreSQL checks
// EXECUTE on a function when the plan starts, whatever branch would call
// it, so a server that rejects it gets statements without it (noSysidExpr)
// rather than a guard inside one statement: a cached generic plan would
// fail on every call.
const (
	sysidExpr   = `(SELECT system_identifier FROM pg_catalog.pg_control_system())`
	noSysidExpr = `NULL::bigint`
)

// acquireSQLWith takes a free or expired lease (bumping the epoch) or
// renews the caller's own; a live lease of another holder is left alone
// and no row is returned. The row lock serializes contenders: a second
// contender re-evaluates the WHERE against the winner's row and gets
// nothing. The control identity is written in the same statement, so
// recording it costs no extra round trip inside the tick bound.
func acquireSQLWith(sysid string) string {
	return `/* pg_sage */ INSERT INTO sage.fleet_leader_lease AS l
	(scope, holder, epoch, acquired_at, renewed_at, expires_at,
	 control_system_identifier, control_datname)
	VALUES ($1, $2, 1, now(), now(), now() + $3::float8 * interval '1 second',
	        ` + sysid + `, current_database())
	ON CONFLICT (scope) DO UPDATE SET
	  control_system_identifier = EXCLUDED.control_system_identifier,
	  control_datname = EXCLUDED.control_datname,
	  holder = EXCLUDED.holder,
	  epoch = CASE WHEN l.holder = EXCLUDED.holder THEN l.epoch ELSE l.epoch + 1 END,
	  acquired_at = CASE WHEN l.holder = EXCLUDED.holder THEN l.acquired_at
	                     ELSE now() END,
	  renewed_at = now(),
	  expires_at = EXCLUDED.expires_at
	WHERE l.holder = EXCLUDED.holder OR l.expires_at <= now()
	RETURNING holder, epoch, expires_at,
	  COALESCE(control_system_identifier::text, ''), control_datname`
}

// currentSQLWith reads scope's lease and names the database it lives in.
func currentSQLWith(sysid string) string {
	return `/* pg_sage */ SELECT holder, epoch, expires_at,
	COALESCE(` + sysid + `::text, ''), current_database()::text
	FROM sage.fleet_leader_lease WHERE scope = $1`
}

var (
	acquireSQL        = acquireSQLWith(sysidExpr)
	acquireNoSysidSQL = acquireSQLWith(noSysidExpr)
	currentSQL        = currentSQLWith(sysidExpr)
	currentNoSysidSQL = currentSQLWith(noSysidExpr)
)

// Acquire takes or renews scope's lease for holder.
func (p *PostgresStore) Acquire(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	if p.pool == nil {
		return Lease{}, false, fmt.Errorf("acquire lease: %w", errNoPool)
	}
	var l Lease
	err := p.withSysid(func(sysid bool) error {
		q := acquireNoSysidSQL
		if sysid {
			q = acquireSQL
		}
		l = Lease{}
		return p.pool.QueryRow(ctx, q, scope, holder, ttl.Seconds()).Scan(&l.Holder,
			&l.Epoch, &l.ExpiresAt, &l.Control.SystemIdentifier, &l.Control.Datname)
	})
	if err == nil {
		return l, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, fmt.Errorf("acquire lease %s: %w", scope, err)
	}
	cur, _, err := p.Current(ctx, scope)
	if err != nil {
		return Lease{}, false, err
	}
	return cur, false, nil
}

// withSysid runs q with the system identifier unless the server rejected
// it before. A rejection (42501 or 0A000) switches to the statements
// without it and runs q once more; if that fails too (other than with no
// row), the function was not the cause, so the identifier stays on and
// the second error is returned.
func (p *PostgresStore) withSysid(q func(sysid bool) error) error {
	err := q(!p.noSysid.Load())
	if !isSysidRejected(err) || p.noSysid.Swap(true) {
		return err
	}
	if err = q(false); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		p.noSysid.Store(false)
	}
	return err
}

// isSysidRejected reports a server error that pg_control_system() raises
// where a provider revokes or restricts it.
func isSysidRejected(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42501" || pgErr.Code == "0A000"
}

// Current is scope's lease; ok is false when there is none.
func (p *PostgresStore) Current(ctx context.Context, scope string) (Lease, bool, error) {
	if p.pool == nil {
		return Lease{}, false, fmt.Errorf("read lease: %w", errNoPool)
	}
	var l Lease
	err := p.withSysid(func(sysid bool) error {
		q := currentNoSysidSQL
		if sysid {
			q = currentSQL
		}
		l = Lease{}
		return p.pool.QueryRow(ctx, q, scope).Scan(&l.Holder, &l.Epoch, &l.ExpiresAt,
			&l.Control.SystemIdentifier, &l.Control.Datname)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("read lease %s: %w", scope, err)
	}
	return l, true, nil
}

// Release expires holder's lease of scope now; another holder's lease is
// untouched.
func (p *PostgresStore) Release(ctx context.Context, scope, holder string) error {
	if p.pool == nil {
		return fmt.Errorf("release lease: %w", errNoPool)
	}
	_, err := p.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.fleet_leader_lease
		SET expires_at = now() WHERE scope = $1 AND holder = $2`, scope, holder)
	if err != nil {
		return fmt.Errorf("release lease %s: %w", scope, err)
	}
	return nil
}
