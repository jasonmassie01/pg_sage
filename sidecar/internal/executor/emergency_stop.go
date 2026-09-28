package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EmergencyStopActorSystem attributes emergency-stop transitions that no
// signed-in user requested, such as a stop restored from an unreadable flag.
const EmergencyStopActorSystem = "system"

// EmergencyStopState is the persisted emergency_stop row.
type EmergencyStopState struct {
	Stopped   bool
	UpdatedBy string
	UpdatedAt time.Time
}

// ReadEmergencyStop returns the persisted emergency_stop flag and who last
// changed it. A missing row means the flag was never set: not stopped.
func ReadEmergencyStop(
	ctx context.Context, pool *pgxpool.Pool,
) (EmergencyStopState, error) {
	if pool == nil {
		return EmergencyStopState{}, errors.New("reading emergency_stop: no pool")
	}
	var value, by string
	var at time.Time
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT value, COALESCE(updated_by, ''), updated_at
		 FROM sage.config WHERE key = 'emergency_stop'`,
	).Scan(&value, &by, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmergencyStopState{}, nil
	}
	if err != nil {
		return EmergencyStopState{}, fmt.Errorf("reading emergency_stop: %w", err)
	}
	return EmergencyStopState{Stopped: value == "true", UpdatedBy: by, UpdatedAt: at}, nil
}

// CheckEmergencyStop queries sage.config for the emergency_stop flag.
// Returns true (stopped) if the value is "true".
//
// It fails CLOSED: only a genuine "no flag set" result (ErrNoRows)
// means "not stopped". Any other error — a transient connection blip,
// statement timeout, lock contention — returns true so that a database
// hiccup can never silently bypass an active emergency stop. The
// kill-switch must not depend on the database being healthy (H7).
func CheckEmergencyStop(ctx context.Context, pool *pgxpool.Pool) bool {
	state, err := ReadEmergencyStop(ctx, pool)
	if err != nil {
		return true
	}
	return state.Stopped
}

// SetEmergencyStop upserts the emergency_stop flag in sage.config as actor
// and appends the transition to sage.config_audit in the same statement, so
// no durable transition exists without its audit row.
func SetEmergencyStop(
	ctx context.Context, pool *pgxpool.Pool, stopped bool, actor string,
) error {
	val := "false"
	if stopped {
		val = "true"
	}
	if actor == "" {
		return fmt.Errorf("setting emergency_stop to %s: actor is required", val)
	}
	_, err := pool.Exec(ctx,
		`/* pg_sage */ WITH prev AS (
			SELECT value FROM sage.config
			WHERE key = 'emergency_stop' AND COALESCE(database_id, 0) = 0
		), upsert AS (
			INSERT INTO sage.config (key, value, updated_at, updated_by)
			VALUES ('emergency_stop', $1, now(), $2)
			ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE
			SET value = $1, updated_at = now(), updated_by = $2
			RETURNING value
		)
		INSERT INTO sage.config_audit (key, old_value, new_value, changed_by_actor)
		SELECT 'emergency_stop', (SELECT value FROM prev), upsert.value, $2
		FROM upsert`,
		val, actor,
	)
	if err != nil {
		return fmt.Errorf("setting emergency_stop to %s: %w", val, err)
	}
	return nil
}
