package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The post-action monitor judges times the database stamped:
// action_log.executed_at and the captured_at of query_store samples and
// sampled plans (DEFAULT now()). On a managed platform the sidecar and the
// database are different machines whose clocks differ, so a window bounded
// by the host's clock drops the newest samples when the database clock
// leads (executor-31). The monitor reads the database clock instead, and
// falls back to the host's only when that read fails, saying why.

// monitorClockSQL reads the database server's clock: clock_timestamp()
// moves inside a transaction, unlike now().
const monitorClockSQL = `/* pg_sage executor */ SELECT clock_timestamp()`

// monitorClockTimeout bounds one read of the database clock.
const monitorClockTimeout = 5 * time.Second

// Where a monitor's time came from.
const (
	clockSourceDatabase = "database"
	clockSourceHost     = "host"
)

// dbClock reads the clock of the database that stamps what the monitor
// judges.
type dbClock func(context.Context) (time.Time, error)

// poolClock reads pool's server clock, bounded by monitorClockTimeout.
func poolClock(pool *pgxpool.Pool) dbClock {
	return func(ctx context.Context) (time.Time, error) {
		if pool == nil {
			return time.Time{}, errors.New(
				"read the database clock (clock_timestamp()): no database pool")
		}
		ctx, cancel := context.WithTimeout(ctx, monitorClockTimeout)
		defer cancel()
		var at time.Time
		if err := pool.QueryRow(ctx, monitorClockSQL).Scan(&at); err != nil {
			return time.Time{}, fmt.Errorf("read the database clock (clock_timestamp()): %w",
				err)
		}
		return at, nil
	}
}

// monitorTime is one reading of the clock a monitor pass judges by.
type monitorTime struct {
	at     time.Time
	source string // clockSourceDatabase or clockSourceHost
	// fallback is why the host clock was used ("" on the database clock).
	fallback string
}

// readMonitorTime reads clock. A failed read, or one that returns no
// time, falls back to the host clock with the reason.
func readMonitorTime(ctx context.Context, clock dbClock) monitorTime {
	if clock == nil {
		return hostMonitorTime("no database clock configured")
	}
	at, err := clock(ctx)
	if err == nil && at.IsZero() {
		err = errors.New("the database clock returned no time")
	}
	if err != nil {
		return hostMonitorTime(err.Error())
	}
	return monitorTime{at: at, source: clockSourceDatabase}
}

func hostMonitorTime(reason string) monitorTime {
	return monitorTime{at: time.Now(), source: clockSourceHost,
		fallback: "database clock unavailable, judged by the host clock: " + reason}
}

// evidence is the clock a verdict's window was bounded by.
func (m monitorTime) evidence() map[string]any {
	out := map[string]any{"source": m.source, "at": m.at.UTC()}
	if m.fallback != "" {
		out["reason"] = m.fallback
	}
	return out
}

// databaseClock is the clock the monitor of cfg judges pool's samples by.
func (c RollbackMonitorConfig) databaseClock(pool *pgxpool.Pool) dbClock {
	if c.clock != nil {
		return c.clock
	}
	return poolClock(pool)
}

// databaseNow is the monitored database's clock for a window over the
// samples it stamped (what names the caller in the log), or the host's
// when it cannot be read.
func (e *Executor) databaseNow(ctx context.Context, what string) time.Time {
	clock := e.clock
	if clock == nil {
		clock = poolClock(e.pool)
	}
	now := readMonitorTime(ctx, clock)
	if now.fallback != "" {
		e.logFn("executor", "%s: %s", what, now.fallback)
	}
	return now.at
}
