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
)

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
	if err := m.validateAcquire(actor, objects, intent); err != nil {
		return "", err
	}
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("acquire lease connection: %w", err)
	}
	keys, err := acquireAdvisoryLocks(ctx, conn, objects)
	if err != nil {
		conn.Release()
		return "", err
	}
	leaseID, err := newLeaseID()
	if err == nil {
		err = m.persistLease(ctx, conn, leaseID, actor, objects, intent)
	}
	if err != nil {
		releaseAdvisoryLocks(context.WithoutCancel(ctx), conn, keys)
		conn.Release()
		return "", err
	}
	m.mu.Lock()
	m.held[leaseID] = heldLease{conn: conn, keys: keys}
	m.mu.Unlock()
	return leaseID, nil
}

func (m *PostgresLeaseManager) validateAcquire(
	actor string, objects []TargetObject, intent string,
) error {
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
	if len(objects) == 0 {
		return errors.New("lease target objects are required")
	}
	return nil
}

func acquireAdvisoryLocks(
	ctx context.Context, conn *pgxpool.Conn, objects []TargetObject,
) ([]int64, error) {
	keys := make([]int64, 0, len(objects))
	for _, object := range objects {
		key := advisoryObjectKey(object.Canonical)
		var acquired bool
		if err := conn.QueryRow(ctx,
			"SELECT pg_try_advisory_lock($1)", key,
		).Scan(&acquired); err != nil {
			releaseAdvisoryLocks(context.WithoutCancel(ctx), conn, keys)
			return nil, fmt.Errorf("acquire advisory lease: %w", err)
		}
		if !acquired {
			releaseAdvisoryLocks(context.WithoutCancel(ctx), conn, keys)
			return nil, ErrLeaseConflict
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (m *PostgresLeaseManager) persistLease(
	ctx context.Context, conn *pgxpool.Conn, leaseID LeaseID, actor string,
	objects []TargetObject, intent string,
) error {
	if _, err := conn.Exec(ctx, `UPDATE sage.change_lease
		SET state='expired', released_at=now()
		WHERE state='active' AND expires_at <= now()`); err != nil {
		return fmt.Errorf("reclaim expired DDL leases: %w", err)
	}
	for _, object := range objects {
		_, err := conn.Exec(ctx, `INSERT INTO sage.change_lease
			(database_id, object_key, decision_id, holder, intent, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+($6 * interval '1 second'))`,
			m.databaseID, object.Canonical, m.decisionID, string(leaseID), intent,
			m.ttl.Seconds())
		if err != nil {
			_, _ = conn.Exec(context.WithoutCancel(ctx), `UPDATE sage.change_lease
				SET state='released', released_at=now()
				WHERE holder=$1 AND state='active'`, string(leaseID))
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrLeaseConflict
			}
			return fmt.Errorf("persist DDL change lease: %w", err)
		}
	}
	_ = actor
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
