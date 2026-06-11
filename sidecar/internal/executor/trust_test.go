package executor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCheckEmergencyStop_FailsClosedOnError is the H7 regression: a
// transient DB error (here, an unreachable database) must NOT be read as
// "not stopped". The kill-switch fails CLOSED — any error other than
// ErrNoRows returns true so a database hiccup can't bypass an active stop.
func TestCheckEmergencyStop_FailsClosedOnError(t *testing.T) {
	// Port 1 is closed; the pool connects lazily and errors on QueryRow
	// with a connection error (not ErrNoRows).
	cfg, err := pgxpool.ParseConfig(
		"postgres://postgres@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if !CheckEmergencyStop(ctx, pool) {
		t.Error("CheckEmergencyStop returned false (proceed) on a " +
			"connection error; the kill-switch must fail closed (true)")
	}
}

// TestInMaintenanceWindow_TimeRange covers the S4 fix: human-friendly
// "HH:MM-HH:MM" windows (including ones that wrap midnight) are honored,
// while cron and "always" still work and garbage stays out-of-window.
func TestInMaintenanceWindow_TimeRange(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 6, 10, h, m, 0, 0, time.UTC)
	}
	cases := []struct {
		expr string
		now  time.Time
		want bool
	}{
		{"02:00-04:00", at(3, 0), true},
		{"02:00-04:00", at(2, 0), true},  // inclusive start
		{"02:00-04:00", at(4, 0), false}, // exclusive end
		{"02:00-04:00", at(1, 59), false},
		{"22:00-02:00", at(23, 0), true}, // wraps midnight
		{"22:00-02:00", at(1, 0), true},
		{"22:00-02:00", at(12, 0), false},
		{"always", at(12, 0), true},
		{"0 2 * * *", at(2, 30), true}, // cron still works
		{"0 2 * * *", at(5, 0), false},
		{"", at(2, 0), false},
		{"garbage", at(2, 0), false},
	}
	for _, c := range cases {
		if got := inMaintenanceWindowAt(c.expr, c.now); got != c.want {
			t.Errorf("inMaintenanceWindowAt(%q, %s) = %v, want %v",
				c.expr, c.now.Format("15:04"), got, c.want)
		}
	}
}
