package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// executor-04: the fleet canary bounds its wait for a target's verdict by
// the target's own monitor: the action's execution plus its class's
// monitor cap (monitorWindows).

func monitorDeadlineExecutor(t *testing.T) (*Executor, context.Context) {
	t.Helper()
	pool, ctx := requireDB(t)
	cfg := config.DefaultConfig()
	cfg.Trust.RollbackWindowMinutes = 15
	cfg.Verify.WindowMaxMinutes = 90
	cfg.Verify.DropWindowHours = 48
	return New(pool, cfg, time.Now(), func(string, string, ...any) {}), ctx
}

func insertExecutedAction(t *testing.T, e *Executor, ctx context.Context, sql string,
	executedAt time.Time) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, executed_at) VALUES ('create_index', $1, 'monitoring', $2) RETURNING id`,
		sql, executedAt).Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id = $1", id)
	})
	return id
}

func TestMonitorDeadlineIsExecutionPlusTheClassCap(t *testing.T) {
	e, ctx := monitorDeadlineExecutor(t)
	executed := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		sql  string
		want time.Duration
	}{
		"create index": {"CREATE INDEX CONCURRENTLY idx_dl ON public.dl (a)", 90 * time.Minute},
		"drop index":   {"DROP INDEX CONCURRENTLY public.idx_dl", 48 * time.Hour},
	} {
		id := insertExecutedAction(t, e, ctx, tc.sql, executed)
		got, err := e.MonitorDeadline(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Equal(executed.Add(tc.want)) {
			t.Errorf("%s: deadline %v, want %v", name, got, executed.Add(tc.want))
		}
	}
}

// Boundary: a cap below the first-verdict window is raised to it, so the
// deadline never ends before the monitor's first verdict.
func TestMonitorDeadlineNeverPrecedesTheFirstVerdict(t *testing.T) {
	e, ctx := monitorDeadlineExecutor(t)
	e.cfg.Verify.WindowMaxMinutes = 5
	executed := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	id := insertExecutedAction(t, e, ctx, "CREATE INDEX CONCURRENTLY idx_dl2 ON public.dl (b)",
		executed)
	got, err := e.MonitorDeadline(ctx, id)
	if err != nil || !got.Equal(executed.Add(15*time.Minute)) {
		t.Fatalf("deadline %v (%v), want execution + 15 min", got, err)
	}
}

func TestMonitorDeadlineErrors(t *testing.T) {
	e, ctx := monitorDeadlineExecutor(t)
	_, err := e.MonitorDeadline(ctx, -1)
	if err == nil || !strings.Contains(err.Error(), "action -1") {
		t.Fatalf("missing action: err = %v, want one naming action -1", err)
	}
	var noDB Executor
	if _, err := noDB.MonitorDeadline(context.Background(), 1); err == nil ||
		!strings.Contains(err.Error(), "no database") {
		t.Fatalf("nil pool: err = %v", err)
	}
}
