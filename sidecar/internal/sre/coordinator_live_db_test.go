package sre

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// CHECK-01 through the real loop: a real multi-session blocking chain on
// PostgreSQL, the real probe runner and store; the investigation names
// the exact idle holder and the queued DDL.

func holdSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	conn, err := pgx.Connect(ctx, os.Getenv(testdb.EnvName))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var pid int
	_ = conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = conn.PgConn().Exec(context.Background(), sql).ReadAll()
	}()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_terminate_backend($1)", pid)
		<-done
		_ = conn.Close(context.Background())
	})
	return pid
}

func waitLockWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var got int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got)
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fewer than %d lock waiters", n)
}

func TestCoordinatorLive_IdleHolderChainNamesTheExactBlocker(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	table := pgx.Identifier{fmt.Sprintf("sre_live_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+table) })
	holder := holdSession(t, ctx, pool, "BEGIN; SELECT count(*) FROM "+table+";")
	time.Sleep(200 * time.Millisecond)
	alter := holdSession(t, ctx, pool, "ALTER TABLE "+table+" ADD COLUMN v int")
	waitLockWaiters(t, ctx, pool, 1)
	holdSession(t, ctx, pool, "SELECT count(*) FROM "+table)
	waitLockWaiters(t, ctx, pool, 2)

	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	inv := startAndRun(t, ctx, c, lockTrigger("inc-live"))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.Summary.Subject != fmt.Sprintf("pid %d", holder) {
		t.Fatalf("investigation = %+v, want idle holder pid %d", inv, holder)
	}
	hs, _ := st.Hypotheses(ctx, inv.Scope, inv.ID)
	var ddl HypothesisRecord
	for _, h := range hs {
		if h.Node == "ddl_lock_queue" {
			ddl = h
		}
	}
	if ddl.Status != HypothesisContributing ||
		!strings.Contains(ddl.Support[0].Text, fmt.Sprintf("pid %d", alter)) {
		t.Fatalf("ddl hypothesis = %+v, want the ALTER pid %d contributing", ddl, alter)
	}
	if !strings.Contains(hs[0].OperatorStep, "pg_cancel_backend does not end") {
		t.Fatalf("root operator step %q offers cancellation (CHECK-02)", hs[0].OperatorStep)
	}
}
