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

	// noSysid is set once the server rejects pg_control_system() even
	// though EXECUTE is granted (a managed provider may hook it): leases
	// are then written with the datname alone.
	noSysid atomic.Bool
}

// NewPostgresStore returns a lease store on pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

var errNoPool = errors.New("no connection pool")

// sysidExpr is the control cluster's system identifier, or NULL when $N
// (the "read it" flag) is false or EXECUTE on pg_control_system() is not
// granted. The subquery is an InitPlan, evaluated only when the CASE
// branch is taken, so a restricted role never calls the function.
const sysidExpr = `CASE WHEN $%d::boolean AND has_function_privilege(
	  'pg_catalog.pg_control_system()', 'EXECUTE')
	THEN (SELECT system_identifier FROM pg_catalog.pg_control_system()) END`

// acquireSQL takes a free or expired lease (bumping the epoch) or renews
// the caller's own; a live lease of another holder is left alone and no
// row is returned. The row lock serializes contenders: a second contender
// re-evaluates the WHERE against the winner's row and gets nothing. The
// control identity is written in the same statement, so recording it
// costs no extra round trip inside the tick bound.
var acquireSQL = `/* pg_sage */ INSERT INTO sage.fleet_leader_lease AS l
	(scope, holder, epoch, acquired_at, renewed_at, expires_at,
	 control_system_identifier, control_datname)
	VALUES ($1, $2, 1, now(), now(), now() + $3::float8 * interval '1 second',
	        ` + fmt.Sprintf(sysidExpr, 4) + `, current_database())
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

// currentSQL reads scope's lease and names the database it lives in.
var currentSQL = `/* pg_sage */ SELECT holder, epoch, expires_at,
	COALESCE((` + fmt.Sprintf(sysidExpr, 2) + `)::text, ''),
	current_database()::text
	FROM sage.fleet_leader_lease WHERE scope = $1`

// Acquire takes or renews scope's lease for holder.
func (p *PostgresStore) Acquire(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	if p.pool == nil {
		return Lease{}, false, fmt.Errorf("acquire lease: %w", errNoPool)
	}
	l, err := p.acquireOnce(ctx, scope, holder, ttl)
	if isSysidRejected(err) && !p.noSysid.Swap(true) {
		// Retry without the identifier; if that fails too, the function
		// was not the cause, so keep reading it.
		if l, err = p.acquireOnce(ctx, scope, holder, ttl); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			p.noSysid.Store(false)
		}
	}
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

func (p *PostgresStore) acquireOnce(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, error) {
	var l Lease
	err := p.pool.QueryRow(ctx, acquireSQL, scope, holder, ttl.Seconds(),
		!p.noSysid.Load()).Scan(&l.Holder, &l.Epoch, &l.ExpiresAt,
		&l.Control.SystemIdentifier, &l.Control.Datname)
	return l, err
}

// isSysidRejected reports a server error that pg_control_system() raises
// where a provider restricts it without revoking EXECUTE.
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
	err := p.pool.QueryRow(ctx, currentSQL, scope, !p.noSysid.Load()).Scan(
		&l.Holder, &l.Epoch, &l.ExpiresAt, &l.Control.SystemIdentifier, &l.Control.Datname)
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
