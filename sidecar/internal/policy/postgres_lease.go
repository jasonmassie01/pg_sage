package policy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrLeaseConflict = errors.New("DDL change lease conflicts with an active writer")
	ErrLeaseNotFound = errors.New("DDL change lease not found")
	// ErrLeaseBusy reports that no connection was free to hold a lease within
	// LeaseConnectionWait. Like a conflict, the action parks and retries.
	ErrLeaseBusy = errors.New("DDL change lease unavailable: database connections busy")
)

// LeaseConnectionWait bounds how long acquiring a lease waits for a pool
// connection. A lease holds its connection for the whole change.
const LeaseConnectionWait = 5 * time.Second

type heldLease struct {
	conn *pgxpool.Conn
	keys []int64
}

type PostgresLeaseManager struct {
	pool       *pgxpool.Pool
	databaseID *int
	decisionID int64
	ttl        time.Duration
	mu         sync.Mutex
	held       map[LeaseID]heldLease
}

func NewPostgresLeaseManager(
	pool *pgxpool.Pool, databaseID *int, decisionID int64, ttl time.Duration,
) *PostgresLeaseManager {
	return &PostgresLeaseManager{
		pool: pool, databaseID: databaseID, decisionID: decisionID,
		ttl: ttl, held: make(map[LeaseID]heldLease),
	}
}

func (m *PostgresLeaseManager) AcquireLease(
	ctx context.Context, actor string, objects []TargetObject, intent string,
) (LeaseID, error) {
	keys := make([]leaseKey, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, leaseKey{key: object.Canonical, name: object.Canonical})
	}
	return m.acquireKeys(ctx, actor, keys, intent)
}

// AcquireTyped leases the exact objects targets name: each target's name
// and OID (see TypedTarget.LeaseKeys), recording its kind and identity.
func (m *PostgresLeaseManager) AcquireTyped(
	ctx context.Context, actor string, targets []TypedTarget, intent string,
) (LeaseID, error) {
	return m.acquireKeys(ctx, actor, typedKeys(targets), intent)
}

func (m *PostgresLeaseManager) acquireKeys(
	ctx context.Context, actor string, keys []leaseKey, intent string,
) (LeaseID, error) {
	if err := m.validateAcquire(actor, len(keys), intent); err != nil {
		return "", err
	}
	conn, err := m.acquireConn(ctx)
	if err != nil {
		return "", err
	}
	held, err := m.acquireAdvisoryLocks(ctx, conn, keys)
	if err != nil {
		conn.Release()
		return "", err
	}
	leaseID, err := newLeaseID()
	if err == nil {
		err = m.persistLease(ctx, conn, leaseID, actor, keys, intent)
	}
	if err != nil {
		releaseAdvisoryLocks(context.WithoutCancel(ctx), conn, held)
		conn.Release()
		return "", err
	}
	m.mu.Lock()
	m.held[leaseID] = heldLease{conn: conn, keys: held}
	m.mu.Unlock()
	return leaseID, nil
}

// acquireConn waits at most LeaseConnectionWait for a pool connection. An
// exhausted pool is a busy lease (the action parks), unless the caller's
// own context ended first.
func (m *PostgresLeaseManager) acquireConn(ctx context.Context) (*pgxpool.Conn, error) {
	waitCtx, cancel := context.WithTimeout(ctx, LeaseConnectionWait)
	defer cancel()
	conn, err := m.pool.Acquire(waitCtx)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("acquire lease connection: %w", ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w (waited %s)", ErrLeaseBusy, LeaseConnectionWait)
	}
	return nil, fmt.Errorf("acquire lease connection: %w", err)
}

func (m *PostgresLeaseManager) validateAcquire(actor string, keys int, intent string) error {
	if m == nil || m.pool == nil {
		return errors.New("lease manager database is unavailable")
	}
	if m.decisionID <= 0 {
		return errors.New("lease decision ID is required")
	}
	if m.ttl <= 0 {
		return errors.New("lease TTL must be positive")
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(intent) == "" {
		return errors.New("lease actor and intent are required")
	}
	if keys == 0 {
		return errors.New("lease target objects are required")
	}
	return nil
}

func (m *PostgresLeaseManager) acquireAdvisoryLocks(
	ctx context.Context, conn *pgxpool.Conn, keys []leaseKey,
) ([]int64, error) {
	held := make([]int64, 0, len(keys))
	for _, key := range keys {
		advisory := advisoryObjectKey(key.key)
		var acquired bool
		if err := conn.QueryRow(ctx,
			"SELECT pg_try_advisory_lock($1)", advisory,
		).Scan(&acquired); err != nil {
			releaseAdvisoryLocks(context.WithoutCancel(ctx), conn, held)
			return nil, fmt.Errorf("acquire advisory lease: %w", err)
		}
		if !acquired {
			releaseAdvisoryLocks(context.WithoutCancel(ctx), conn, held)
			return nil, m.conflict(ctx, conn, key.key)
		}
		held = append(held, advisory)
	}
	return held, nil
}

func (m *PostgresLeaseManager) persistLease(
	ctx context.Context, conn *pgxpool.Conn, leaseID LeaseID, actor string,
	keys []leaseKey, intent string,
) error {
	if _, err := conn.Exec(ctx, `UPDATE sage.change_lease
		SET state='expired', released_at=now()
		WHERE state='active' AND expires_at <= now()`); err != nil {
		return fmt.Errorf("reclaim expired DDL leases: %w", err)
	}
	for _, key := range keys {
		_, err := conn.Exec(ctx, `INSERT INTO sage.change_lease
			(database_id, object_key, decision_id, holder, intent, expires_at, actor,
			 object_type, object_oid, object_name)
			VALUES ($1,$2,$3,$4,$5,now()+($6 * interval '1 second'),$7,NULLIF($8,''),
			        NULLIF($9::bigint,0)::oid,$10)`,
			m.databaseID, key.key, m.decisionID, string(leaseID), intent,
			m.ttl.Seconds(), actor, key.kind, int64(key.oid), key.name)
		if err != nil {
			_, _ = conn.Exec(context.WithoutCancel(ctx), `UPDATE sage.change_lease
				SET state='released', released_at=now()
				WHERE holder=$1 AND state='active'`, string(leaseID))
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return m.conflict(ctx, conn, key.key)
			}
			return fmt.Errorf("persist DDL change lease: %w", err)
		}
	}
	return nil
}

func (m *PostgresLeaseManager) ReleaseLease(ctx context.Context, id LeaseID) error {
	m.mu.Lock()
	held, ok := m.held[id]
	if ok {
		delete(m.held, id)
	}
	m.mu.Unlock()
	if !ok {
		return ErrLeaseNotFound
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, updateErr := held.conn.Exec(cleanupCtx, `UPDATE sage.change_lease
		SET state='released', released_at=now()
		WHERE holder=$1 AND state='active'`, string(id))
	releaseAdvisoryLocks(cleanupCtx, held.conn, held.keys)
	held.conn.Release()
	if updateErr != nil {
		return fmt.Errorf("release DDL change lease: %w", updateErr)
	}
	return nil
}

func releaseAdvisoryLocks(ctx context.Context, conn *pgxpool.Conn, keys []int64) {
	for index := len(keys) - 1; index >= 0; index-- {
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", keys[index])
	}
}

func advisoryObjectKey(canonical string) int64 {
	digest := sha256.Sum256([]byte(canonical))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func newLeaseID() (LeaseID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate lease ID: %w", err)
	}
	return LeaseID("lease_" + hex.EncodeToString(raw[:])), nil
}

var _ LeaseManager = (*PostgresLeaseManager)(nil)
