package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// inMaintenanceWindow reports whether the wall clock is inside the
// configured trust.maintenance_window.
func inMaintenanceWindow(expression string) bool {
	return inMaintenanceWindowAt(expression, time.Now())
}

// inMaintenanceWindowAt evaluates trust.maintenance_window with the single
// window engine, policy.ParseWindow. Unset and the config-only "never"
// aliases are closed. A value that does not parse (possible only for an
// override stored before validation existed) fails closed.
func inMaintenanceWindowAt(expression string, now time.Time) bool {
	if strings.TrimSpace(expression) == "" || config.MaintenanceWindowDisabled(expression) {
		return false
	}
	window, err := policy.ParseWindow(expression)
	return err == nil && window.Contains(now)
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
	if pool == nil {
		return true // the flag cannot be read: fail closed
	}
	var value string
	err := pool.QueryRow(ctx,
		"SELECT value FROM sage.config WHERE key = 'emergency_stop'",
	).Scan(&value)
	if err != nil {
		// No row → the flag was never set → not stopped.
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		// Unknown/transient error → fail closed.
		return true
	}
	return value == "true"
}

// SetEmergencyStop upserts the emergency_stop flag in sage.config.
func SetEmergencyStop(
	ctx context.Context, pool *pgxpool.Pool, stopped bool,
) error {
	val := "false"
	if stopped {
		val = "true"
	}

	_, err := pool.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config (key, value, updated_at, updated_by)
		 VALUES ('emergency_stop', $1, now(), 'executor')
		 ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE
		 SET value = $1, updated_at = now(), updated_by = 'executor'`,
		val,
	)
	if err != nil {
		return fmt.Errorf("setting emergency_stop to %s: %w", val, err)
	}
	return nil
}
