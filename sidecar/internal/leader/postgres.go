package leader

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
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

	mu       sync.Mutex
	identity *ControlIdentity // resolved on first use, then fixed
}

// NewPostgresStore returns a lease store on pool.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

var errNoPool = errors.New("no connection pool")

// acquireSQL takes a free or expired lease (bumping the epoch) or renews
// the caller's own; a live lease of another holder is left alone and no
// row is returned. The row lock serializes contenders: a second contender
// re-evaluates the WHERE against the winner's row and gets nothing.
const acquireSQL = `/* pg_sage */ INSERT INTO sage.fleet_leader_lease AS l
	(scope, holder, epoch, acquired_at, renewed_at, expires_at,
	 control_system_identifier, control_datname)
	VALUES ($1, $2, 1, now(), now(), now() + $3::float8 * interval '1 second',
	        $4::text::bigint, current_database())
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
	RETURNING holder, epoch, expires_at`

const currentSQL = `/* pg_sage */ SELECT holder, epoch, expires_at
	FROM sage.fleet_leader_lease WHERE scope = $1`

// Acquire takes or renews scope's lease for holder.
func (p *PostgresStore) Acquire(ctx context.Context, scope, holder string,
	ttl time.Duration) (Lease, bool, error) {
	if p.pool == nil {
		return Lease{}, false, fmt.Errorf("acquire lease: %w", errNoPool)
	}
	id, err := p.controlIdentity(ctx)
	if err != nil {
		return Lease{}, false, err
	}
	var sysid *string
	if id.SystemIdentifier != "" {
		sysid = &id.SystemIdentifier
	}
	l := Lease{Control: id}
	err = p.pool.QueryRow(ctx, acquireSQL, scope, holder, ttl.Seconds(), sysid).
		Scan(&l.Holder, &l.Epoch, &l.ExpiresAt)
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
	cur.Control = id
	return cur, false, nil
}

const datnameSQL = `/* pg_sage */ SELECT current_database()::text`

const systemIdentifierSQL = `/* pg_sage */ SELECT system_identifier::text
	FROM pg_control_system()`

// controlIdentity names the database this store keeps leases in. A server
// that rejects pg_control_system() (a managed provider may restrict it)
// leaves the system identifier empty; a failed round trip is an error and
// is retried on the next call.
func (p *PostgresStore) controlIdentity(ctx context.Context) (ControlIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.identity != nil {
		return *p.identity, nil
	}
	var id ControlIdentity
	if err := p.pool.QueryRow(ctx, datnameSQL).Scan(&id.Datname); err != nil {
		return ControlIdentity{}, fmt.Errorf("read control database name: %w", err)
	}
	err := p.pool.QueryRow(ctx, systemIdentifierSQL).Scan(&id.SystemIdentifier)
	var pgErr *pgconn.PgError
	if err != nil && !errors.As(err, &pgErr) {
		return ControlIdentity{}, fmt.Errorf("read control system identifier: %w", err)
	}
	if err != nil {
		id.SystemIdentifier = ""
	} else if _, perr := strconv.ParseInt(id.SystemIdentifier, 10, 64); perr != nil {
		return ControlIdentity{}, fmt.Errorf("control system identifier %q: %w",
			id.SystemIdentifier, perr)
	}
	p.identity = &id
	return id, nil
}

// Current is scope's lease; ok is false when there is none.
func (p *PostgresStore) Current(ctx context.Context, scope string) (Lease, bool, error) {
	if p.pool == nil {
		return Lease{}, false, fmt.Errorf("read lease: %w", errNoPool)
	}
	var l Lease
	err := p.pool.QueryRow(ctx, currentSQL, scope).Scan(&l.Holder, &l.Epoch, &l.ExpiresAt)
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
