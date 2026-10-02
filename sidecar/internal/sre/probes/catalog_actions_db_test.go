package probes

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The action probes against a real active blocker: a running statement
// holds ACCESS SHARE, an ALTER TABLE queues behind it and a reader queues
// behind the ALTER.

type activeChain struct {
	table       string
	holderPID   int
	holderStart time.Time
	holderQuery string
	alterPID    int
	alterStart  time.Time
	// cancel cancels the holder's statement and waits for the chain to
	// drain; the sessions stay connected until cleanup.
	cancel func()
}

func startActiveChain(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *activeChain {
	t.Helper()
	dsn := os.Getenv(testdb.EnvName)
	ac := &activeChain{table: fmt.Sprintf("sre_active_%d", time.Now().UnixNano())}
	ident := pgx.Identifier{ac.table}.Sanitize()
	for _, sql := range []string{"CREATE TABLE " + ident + " (id int)",
		"INSERT INTO " + ident + " VALUES (1)"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	holder := dialLive(t, ctx, dsn)
	_ = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&ac.holderPID)
	ac.holderQuery = "SELECT pg_sleep(20) FROM " + ident
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = holder.Exec(context.Background(), ac.holderQuery) }()
	waitActive(t, ctx, pool, ac.holderPID)
	conns := []*pgx.Conn{holder}
	for i, sql := range []string{"ALTER TABLE " + ident + " ADD COLUMN v int",
		"SELECT count(*) FROM " + ident} {
		c := dialLive(t, ctx, dsn)
		if i == 0 {
			_ = c.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&ac.alterPID)
		}
		conns = append(conns, c)
		wg.Add(1)
		go func(sql string) { defer wg.Done(); _, _ = c.Exec(context.Background(), sql) }(sql)
		waitLockWaiters(t, ctx, pool, i+1)
	}
	_ = pool.QueryRow(ctx, `SELECT backend_start FROM pg_stat_activity WHERE pid = $1`,
		ac.holderPID).Scan(&ac.holderStart)
	_ = pool.QueryRow(ctx, `SELECT backend_start FROM pg_stat_activity WHERE pid = $1`,
		ac.alterPID).Scan(&ac.alterStart)
	var once sync.Once
	ac.cancel = func() {
		once.Do(func() {
			_, _ = pool.Exec(context.Background(), "SELECT pg_cancel_backend($1)",
				ac.holderPID)
			wg.Wait()
		})
	}
	t.Cleanup(func() {
		ac.cancel()
		for _, c := range conns {
			_ = c.Close(context.Background())
		}
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
	})
	return ac
}

func waitActive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		var state string
		_ = pool.QueryRow(ctx, `SELECT state FROM pg_stat_activity WHERE pid = $1`,
			pid).Scan(&state)
		if state == "active" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never became active", pid)
}

func TestSignalTargetPinsTheActiveBlocker(t *testing.T) {
	pool, ctx := livePool(t)
	ac := startActiveChain(t, ctx, pool)
	r := NewRunner(pool, ActionRegistry(), NewLimiter(MaxSidecarConcurrency))
	res := r.Run(ctx, SignalTarget, Args{PID: int32(ac.holderPID),
		BackendStart: ac.holderStart})
	rows, err := SignalTargets(res)
	if err != nil || len(rows) != 1 {
		t.Fatalf("signal_target = %+v (%s %s), %v", rows, res.Status, res.Error, err)
	}
	got := rows[0]
	var db, user, hash string
	var queryID int64
	if err := pool.QueryRow(ctx, `SELECT current_database(), current_user,
		encode(sha256(convert_to($1, 'UTF8')), 'hex'),
		COALESCE((SELECT query_id FROM pg_stat_activity WHERE pid = $2), 0)`,
		ac.holderQuery, ac.holderPID).Scan(&db, &user, &hash, &queryID); err != nil {
		t.Fatalf("expected identity: %v", err)
	}
	if got.PID != int32(ac.holderPID) || !got.BackendStart.Equal(ac.holderStart) ||
		got.State != "active" || got.Database != db || got.User != user ||
		got.QueryHash != hash || got.QueryID != queryID || got.Blocking < 1 ||
		!got.InCurrentDatabase || got.InRecovery || got.Waiting ||
		got.BackendType != "client backend" || got.QueryStart.IsZero() {
		t.Fatalf("target = %+v, want db %s user %s hash %s qid %d", got, db, user,
			hash, queryID)
	}
}

func TestSignalTargetIsEmptyForAReusedPID(t *testing.T) {
	pool, ctx := livePool(t)
	ac := startActiveChain(t, ctx, pool)
	r := NewRunner(pool, ActionRegistry(), NewLimiter(MaxSidecarConcurrency))
	res := r.Run(ctx, SignalTarget, Args{PID: int32(ac.holderPID),
		BackendStart: ac.holderStart.Add(-time.Hour)})
	rows, err := SignalTargets(res)
	if err != nil || len(rows) != 0 || res.Status != StatusEmpty {
		t.Fatalf("another incarnation of pid %d = %+v (%s), %v", ac.holderPID, rows,
			res.Status, err)
	}
}

func TestRecoverySampleSeesTheEdgesClear(t *testing.T) {
	pool, ctx := livePool(t)
	ac := startActiveChain(t, ctx, pool)
	r := NewRunner(pool, ActionRegistry(), NewLimiter(MaxSidecarConcurrency))
	args := Args{PID: int32(ac.holderPID), BackendStart: ac.holderStart}
	before, err := RecoveryRows(r.Run(ctx, RecoverySample, args))
	if err != nil {
		t.Fatalf("recovery_sample before: %v", err)
	}
	if !hasRow(before, ac.holderPID, func(r RecoveryRow) bool { return r.IsTarget }) ||
		!hasRow(before, ac.alterPID, func(r RecoveryRow) bool {
			return r.Waiting && r.BlockedByTarget && r.BackendStart.Equal(ac.alterStart)
		}) {
		t.Fatalf("before the cancel: %+v", before)
	}
	ac.cancel()
	after, err := RecoveryRows(r.Run(ctx, RecoverySample, args))
	if err != nil {
		t.Fatalf("recovery_sample after: %v", err)
	}
	if !hasRow(after, ac.holderPID, func(r RecoveryRow) bool {
		return r.IsTarget && r.State != "active"
	}) || !hasRow(after, ac.alterPID, func(r RecoveryRow) bool { return !r.Waiting }) {
		t.Fatalf("after the cancel the target and the ALTER session are not idle: %+v",
			after)
	}
	for _, row := range after {
		if row.BlockedByTarget || row.Waiting {
			t.Fatalf("after the cancel a row still blocks or waits: %+v", after)
		}
	}
}

func hasRow(rows []RecoveryRow, pid int, ok func(RecoveryRow) bool) bool {
	for _, r := range rows {
		if int(r.PID) == pid && ok(r) {
			return true
		}
	}
	return false
}
