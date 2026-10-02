package earned

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
)

// PostgresConcurrency counts other pg_sage actions on an object in the
// monitored database: active change leases and actions executed within a
// window (their verification may still be running).
type PostgresConcurrency struct {
	pool       *pgxpool.Pool
	databaseID *int
}

// NewPostgresConcurrency reads leases and actions of databaseID (nil for
// a standalone database) from pool.
func NewPostgresConcurrency(pool *pgxpool.Pool, databaseID *int) *PostgresConcurrency {
	return &PostgresConcurrency{pool: pool, databaseID: databaseID}
}

var _ ConcurrencySource = (*PostgresConcurrency)(nil)

// ConcurrentActions counts other writers on targets. leaseHeld skips the
// lease check: the caller holds the object's only active lease itself.
func (c *PostgresConcurrency) ConcurrentActions(ctx context.Context, targets []string,
	leaseHeld bool, window time.Duration) (int, error) {
	if c == nil || c.pool == nil {
		return 0, errors.New("concurrency source has no database")
	}
	if len(targets) == 0 {
		return 0, errors.New("no target objects to check")
	}
	keys := leaseKeys(targets)
	var leases, recent int
	if !leaseHeld && len(keys) > 0 {
		if err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT count(*)
			FROM sage.change_lease
			WHERE state = 'active' AND expires_at > now()
			  AND COALESCE(database_id, 0) = COALESCE($1::bigint, 0)
			  AND object_key = ANY($2)`, c.databaseID, keys).Scan(&leases); err != nil {
			return 0, fmt.Errorf("count active change leases: %w", err)
		}
	}
	if err := c.pool.QueryRow(ctx, `/* pg_sage */ SELECT count(*)
		FROM sage.action_log al JOIN sage.decision d ON d.id = al.decision_id
		WHERE al.executed_at > now() - make_interval(secs => $1::double precision)
		  AND COALESCE(d.database_id, 0) = COALESCE($2::bigint, 0)
		  AND d.target_objects ?| $3`, window.Seconds(), c.databaseID,
		targets).Scan(&recent); err != nil {
		return 0, fmt.Errorf("count recent actions on the targets: %w", err)
	}
	return leases + recent, nil
}

// leaseKeys are the canonical lease keys of the schema-qualified targets
// (others, like "slot:x", hold no lease).
func leaseKeys(targets []string) []string {
	var keys []string
	for _, t := range targets {
		objects, err := policy.NormalizeTargetObjects([]string{t})
		if err != nil || len(objects) != 1 {
			continue
		}
		keys = append(keys, objects[0].Canonical)
	}
	return keys
}
