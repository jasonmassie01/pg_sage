package executor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MonitorDeadline is when the post-action monitor of an action stops
// watching it: the action's execution plus its class's monitor cap
// (monitorWindows), under the executor's current verify settings. A fleet
// canary waits for the action's verdict until then (plus a grace).
func (e *Executor) MonitorDeadline(ctx context.Context, actionID int64) (time.Time, error) {
	if e.pool == nil {
		return time.Time{}, errors.New("monitor deadline: the executor has no database")
	}
	var sql string
	var executedAt time.Time
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT sql_executed, executed_at
		FROM sage.action_log WHERE id = $1`, actionID).Scan(&sql, &executedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("read action %d for its monitor deadline: %w",
			actionID, err)
	}
	cfg := e.rollbackMonitorConfig(nil) // only the windows are read
	_, capW, _ := monitorWindows(verificationClass(sql), cfg)
	return executedAt.Add(capW), nil
}
