package sre

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The whole M5 path against real PostgreSQL: a running statement blocks
// an ALTER TABLE and a reader; a real investigation concludes; the action
// service proposes an evidence-matched cancel; a human approves it in the
// existing queue; the real executor and policy gate (staffed profile, so
// outside its maintenance window) cancel exactly that statement; recovery
// is verified over fresh samples.

type liveChain struct {
	table     string
	holderPID int
	alterPID  int
	holderErr chan error
	conns     []*pgx.Conn
	wg        sync.WaitGroup
}

// startLiveChain runs holderSQL (in order) on one session and queues an
// ALTER TABLE and a reader behind the first statement.
func startLiveChain(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	holderSQL ...string) *liveChain {
	t.Helper()
	dsn := os.Getenv(testdb.EnvName)
	lc := &liveChain{table: fmt.Sprintf("sre_m5_%d", time.Now().UnixNano()),
		holderErr: make(chan error, len(holderSQL))}
	ident := pgx.Identifier{lc.table}.Sanitize()
	for _, sql := range []string{"CREATE TABLE " + ident + " (id int)",
		"INSERT INTO " + ident + " VALUES (1)"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	holder := lc.dial(t, ctx, dsn)
	_ = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&lc.holderPID)
	lc.wg.Add(1)
	go func() {
		defer lc.wg.Done()
		for _, sql := range holderSQL {
			_, err := holder.Exec(context.Background(),
				strings.ReplaceAll(sql, "$TABLE", ident))
			lc.holderErr <- err
		}
	}()
	waitState(t, ctx, pool, lc.holderPID, "active")
	for i, sql := range []string{"ALTER TABLE " + ident + " ADD COLUMN v int",
		"SELECT count(*) FROM " + ident} {
		c := lc.dial(t, ctx, dsn)
		if i == 0 {
			_ = c.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&lc.alterPID)
		}
		lc.wg.Add(1)
		go func(sql string) { defer lc.wg.Done(); _, _ = c.Exec(context.Background(), sql) }(sql)
		waitWaiters(t, ctx, pool, i+1)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_cancel_backend($1)", lc.holderPID)
		lc.wg.Wait()
		for _, c := range lc.conns {
			_ = c.Close(context.Background())
		}
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
	})
	return lc
}

func (lc *liveChain) dial(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	lc.conns = append(lc.conns, c)
	return c
}

func waitState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var state string
		_ = pool.QueryRow(ctx, `SELECT state FROM pg_stat_activity WHERE pid = $1`,
			pid).Scan(&state)
		if state == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never reached state %q", pid, want)
}

func waitWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lock chain never reached %d waiters", want)
}

type liveActions struct {
	h       *actionHarness
	exec    *executor.Executor
	actions *ActionService
}

// newLiveActions investigates the live chain with real probes and wires
// the action service to the real executor behind the staffed policy.
func newLiveActions(t *testing.T, st *PostgresStore, pool *pgxpool.Pool,
	ctx context.Context) *liveActions {
	t.Helper()
	coord, _ := testCoordinator(t, ctx, st, probes.NewRunner(pool, probes.Catalog(), nil),
		nil)
	inv := startAndRun(t, ctx, coord, lockTrigger(string(NewUUID())))
	if inv.State != StateConcluded || inv.Summary.Root != "ddl_lock_queue" {
		t.Fatalf("live investigation = %s root %q (%s)", inv.State, inv.Summary.Root,
			inv.Summary.Reason)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	exec := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	exec.EnableStandingPolicyDocument(policy.StaffedProfile(), nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	ac := DefaultActionConfig()
	ac.RequestApproval = false
	ac.RecoveryInterval, ac.RecoverySamples = 300*time.Millisecond, 3
	svc := NewService("orders", coord, st)
	actions, err := NewActionService(ActionDeps{Service: svc,
		Targets: probes.NewRunner(pool, probes.ActionRegistry(), nil),
		Queue:   NewPGApprovalQueue(pool, nil), Executor: exec, Config: ac,
		LogFn: func(string, string, ...any) {}, Now: time.Now})
	if err != nil {
		t.Fatalf("NewActionService: %v", err)
	}
	exec.SetApprovedActionRunner(actions)
	h := &actionHarness{st: st, pool: pool, ctx: ctx, coord: coord, svc: svc, inv: inv,
		actions: actions, as: store.NewActionStore(pool), cfg: ac}
	return &liveActions{h: h, exec: exec, actions: actions}
}

func TestLiveApprovedCancelRecoversTheChain(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	lc := startLiveChain(t, ctx, pool, "SELECT pg_sleep(30) FROM $TABLE")
	la := newLiveActions(t, st, pool, ctx)
	p := la.h.requested(t)
	if p.Target == nil || int(p.Target.PID) != lc.holderPID {
		t.Fatalf("proposal target = %+v, want holder %d", p.Target, lc.holderPID)
	}
	approved := la.h.approved(t, p, 1)
	run, err := la.exec.RunApprovedAction(ctx, approved, 1)
	if err != nil {
		t.Fatalf("approved run: %v", err)
	}
	select {
	case err := <-lc.holderErr:
		if err == nil || !strings.Contains(err.Error(), "canceling statement") {
			t.Fatalf("holder error = %v, want a cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the holder's statement was not cancelled")
	}
	var actionType string
	var decision *int64
	if err := pool.QueryRow(ctx, `SELECT action_type, decision_id FROM sage.action_log
		WHERE id = $1`, run.ActionLogID).Scan(&actionType, &decision); err != nil ||
		actionType != "cancel_backend" || decision == nil {
		t.Fatalf("action_log = %s decision %v (%v)", actionType, decision, err)
	}
	var got Proposal
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if err := la.actions.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		got = la.h.proposal(t, p.ID)
		if got.Recovery.State != RecoveryObserving {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.State != ProposalExecuted || got.Recovery.State != RecoveryRecovered {
		t.Fatalf("live outcome = %s / %+v", got.State, got.Recovery)
	}
}

// CHECK-19 live: the approved target finished and the same session runs a
// new statement; the recheck refuses, and the new statement keeps running.
func TestLiveApprovedCancelRefusesTheSessionsNextQuery(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	lc := startLiveChain(t, ctx, pool, "SELECT pg_sleep(4) FROM $TABLE",
		"SELECT pg_sleep(20) FROM $TABLE")
	la := newLiveActions(t, st, pool, ctx)
	p := la.h.requested(t)
	approved := la.h.approved(t, p, 1)
	select {
	case err := <-lc.holderErr:
		if err != nil {
			t.Fatalf("first statement: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the first statement never finished")
	}
	waitState(t, ctx, pool, lc.holderPID, "active")
	_, err := la.exec.RunApprovedAction(ctx, approved, 1)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("run against the next query = %v, want a stale-evidence refusal", err)
	}
	got := la.h.proposal(t, p.ID)
	if got.State != ProposalRefused || got.Reason != ReasonTargetChanged {
		t.Fatalf("proposal = %s/%s, want refused target_changed", got.State, got.Reason)
	}
	select {
	case err := <-lc.holderErr:
		t.Fatalf("the session's next statement ended early: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
}
