package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
)

const (
	// FleetStopKey is the meta database's sage.config row that keeps the
	// fleet-wide emergency stop across restarts in meta-db mode.
	FleetStopKey = "fleet_emergency_stop"
	// FleetScopeKey is each member's sage.config row naming why its
	// emergency_stop is set: 'fleet' (a stop of all databases) or
	// 'database'. YAML fleets have no control database, so a member that
	// restarts with emergency_stop 'true' and scope 'fleet' re-latches the
	// whole fleet.
	FleetScopeKey = "emergency_stop_scope"

	fleetScopeFleet    = "fleet"
	fleetScopeDatabase = "database"
)

// errNoDurableFlag marks an instance without a pool: it has no sage.config
// to read, so there is nothing to restore or reconcile.
var errNoDurableFlag = errors.New("no connection pool: no durable emergency stop flag")

// FleetStopStore persists the fleet-wide emergency stop latch.
type FleetStopStore interface {
	ReadFleetStop(ctx context.Context) (executor.EmergencyStopState, error)
	WriteFleetStop(ctx context.Context, stopped bool, actor string) error
}

type pgFleetStopStore struct {
	pool *pgxpool.Pool
}

// NewPGFleetStopStore keeps the fleet latch in pool's sage.config (the
// meta database). A nil pool returns no store.
func NewPGFleetStopStore(pool *pgxpool.Pool) FleetStopStore {
	if pool == nil {
		return nil
	}
	return &pgFleetStopStore{pool: pool}
}

func (s *pgFleetStopStore) ReadFleetStop(
	ctx context.Context,
) (executor.EmergencyStopState, error) {
	flag, err := readConfigFlag(ctx, s.pool, FleetStopKey)
	if err != nil {
		return executor.EmergencyStopState{}, err
	}
	return executor.EmergencyStopState{
		Stopped: flag.value == "true", UpdatedBy: flag.by, UpdatedAt: flag.at,
	}, nil
}

func (s *pgFleetStopStore) WriteFleetStop(
	ctx context.Context, stopped bool, actor string,
) error {
	value := "false"
	if stopped {
		value = "true"
	}
	return writeConfigFlag(ctx, s.pool, FleetStopKey, value, actor)
}

type configFlag struct {
	value string
	by    string
	at    time.Time
}

// readConfigFlag reads one global sage.config row; a missing row reads as
// the zero flag (never set).
func readConfigFlag(
	ctx context.Context, pool *pgxpool.Pool, key string,
) (configFlag, error) {
	var flag configFlag
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT value, COALESCE(updated_by, ''), updated_at
		 FROM sage.config WHERE key = $1 AND COALESCE(database_id, 0) = 0`,
		key,
	).Scan(&flag.value, &flag.by, &flag.at)
	if errors.Is(err, pgx.ErrNoRows) {
		return configFlag{}, nil
	}
	if err != nil {
		return configFlag{}, fmt.Errorf("reading %s: %w", key, err)
	}
	return flag, nil
}

// writeConfigFlag upserts one global sage.config row as actor and appends
// the transition to sage.config_audit in the same statement.
func writeConfigFlag(
	ctx context.Context, pool *pgxpool.Pool, key, value, actor string,
) error {
	if actor == "" {
		return fmt.Errorf("setting %s to %s: actor is required", key, value)
	}
	_, err := pool.Exec(ctx,
		`/* pg_sage */ WITH prev AS (
			SELECT value FROM sage.config
			WHERE key = $1 AND COALESCE(database_id, 0) = 0
		), upsert AS (
			INSERT INTO sage.config (key, value, updated_at, updated_by)
			VALUES ($1, $2, now(), $3)
			ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE
			SET value = EXCLUDED.value, updated_at = now(),
			    updated_by = EXCLUDED.updated_by
			RETURNING value
		)
		INSERT INTO sage.config_audit (key, old_value, new_value, changed_by_actor)
		SELECT $1, (SELECT value FROM prev), upsert.value, $3
		FROM upsert`,
		key, value, actor,
	)
	if err != nil {
		return fmt.Errorf("setting %s to %s: %w", key, value, err)
	}
	return nil
}

// readMemberScope reports whether a member's emergency_stop was set by a
// stop of the whole fleet. An instance without a pool has no scope row.
func readMemberScope(ctx context.Context, inst *DatabaseInstance) (bool, error) {
	if inst.Pool == nil {
		return false, nil
	}
	flag, err := readConfigFlag(ctx, inst.Pool, FleetScopeKey)
	return flag.value == fleetScopeFleet, err
}

// persistMemberScope writes a member's emergency_stop_scope. An instance
// without a pool has nothing to write: its emergency_stop write has
// already failed and been reported.
func persistMemberScope(
	ctx context.Context, inst *DatabaseInstance, fleet bool, actor string,
) error {
	if inst.Pool == nil {
		return nil
	}
	value := fleetScopeDatabase
	if fleet {
		value = fleetScopeFleet
	}
	return writeConfigFlag(ctx, inst.Pool, FleetScopeKey, value, actor)
}
