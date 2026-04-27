package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// ShouldExecute determines whether a finding should be auto-remediated
// based on trust level, risk tier, ramp age, and maintenance window.
func ShouldExecute(
	f analyzer.Finding,
	cfg *config.Config,
	rampStart time.Time,
	isReplica bool,
	emergencyStop bool,
) bool {
	if emergencyStop || isReplica {
		return false
	}

	rampAge := time.Since(rampStart)

	switch cfg.Trust.Level {
	case "observation":
		return false

	case "advisory":
		// Advisory: only SAFE actions after 8-day ramp.
		return f.ActionRisk == "safe" &&
			cfg.Trust.Tier3Safe &&
			rampAge >= 8*24*time.Hour

	case "autonomous":
		switch f.ActionRisk {
		case "safe":
			return cfg.Trust.Tier3Safe &&
				rampAge >= 8*24*time.Hour
		case "moderate":
			return cfg.Trust.Tier3Moderate &&
				rampAge >= 31*24*time.Hour &&
				inMaintenanceWindow(cfg.Trust.MaintenanceWindow)
		case "high_risk":
			return false
		default:
			return false
		}

	default:
		return false
	}
}

// inMaintenanceWindow parses a simple cron expression (minute hour * * *)
// and returns true if the current time is within a 1-hour window of the
// scheduled time. Wildcards ("*") mean "any" for that field. Supports:
//   - "* * * * *" → always in window
//   - "0 2 * * *" → 02:00-03:00 daily
//   - "30 * * * *" → at minute 30 of every hour (1h window)
//   - "always" → always in window
func inMaintenanceWindow(cronExpr string) bool {
	return inMaintenanceWindowAt(cronExpr, time.Now())
}

func inMaintenanceWindowAt(cronExpr string, now time.Time) bool {
	if cronExpr == "" {
		return false
	}

	trimmed := strings.TrimSpace(cronExpr)
	if strings.EqualFold(trimmed, "always") {
		return true
	}

	parts := strings.Fields(trimmed)
	if len(parts) < 2 {
		return false
	}

	// Parse minute field: "*" means any minute.
	minuteWild := parts[0] == "*"
	var minute int
	if !minuteWild {
		var err error
		minute, err = strconv.Atoi(parts[0])
		if err != nil {
			return false
		}
	}

	// Parse hour field: "*" means any hour.
	hourWild := parts[1] == "*"
	var hour int
	if !hourWild {
		var err error
		hour, err = strconv.Atoi(parts[1])
		if err != nil {
			return false
		}
	}

	// Both wildcards → always in window.
	if minuteWild && hourWild {
		return true
	}

	if hourWild {
		// Any hour, specific minute: 1-hour window starting at :minute.
		windowStart := time.Date(
			now.Year(), now.Month(), now.Day(),
			now.Hour(), minute, 0, 0, now.Location(),
		)
		windowEnd := windowStart.Add(1 * time.Hour)
		return !now.Before(windowStart) && now.Before(windowEnd)
	}

	// Specific hour (minute may be wild or specific).
	if minuteWild {
		minute = 0
	}
	windowStart := time.Date(
		now.Year(), now.Month(), now.Day(),
		hour, minute, 0, 0, now.Location(),
	)
	windowEnd := windowStart.Add(1 * time.Hour)

	return !now.Before(windowStart) && now.Before(windowEnd)
}

// CheckEmergencyStop queries sage.config for the emergency_stop flag.
// Returns true if the value is "true".
func CheckEmergencyStop(ctx context.Context, pool *pgxpool.Pool) bool {
	var value string
	err := pool.QueryRow(ctx,
		"SELECT value FROM sage.config WHERE key = 'emergency_stop'",
	).Scan(&value)
	if err != nil {
		return false
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
		`INSERT INTO sage.config (key, value, updated_at, updated_by)
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
