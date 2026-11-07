package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// unpinnedExecutor is an executor on a fresh database whose role default
// is repeatable read, through a plain pool: a session whose read committed
// pin a transaction pooler lost. Only its own transactions keep the
// action_log write at read committed.
func unpinnedExecutor(t *testing.T, label string) (*Executor, *pgxpool.Pool, *[]string) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", label, err)
	}
	if err := schema.Bootstrap(ctx, boot); err != nil {
		boot.Close()
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	boot.Close()
	testdb.SetIsolationDefault(t, dsn, "repeatable read")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	var mu sync.Mutex
	logs := &[]string{}
	e := New(pool, config.DefaultConfig(), time.Now(), func(_, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		*logs = append(*logs, fmt.Sprintf(msg, args...))
	})
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.Shutdown(sctx)
	})
	return e, pool, logs
}

func actionLogCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.action_log").Scan(&n); err != nil {
		t.Fatalf("count action_log: %v", err)
	}
	return n
}

// insertIsolated inserts one action_log row through q and returns its id
// and the isolation level the insert ran under.
func insertIsolated(ctx context.Context, q actionLogWriter, tag string) (int64, string, error) {
	var id int64
	var level string
	err := q.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', $1) RETURNING id, current_setting('transaction_isolation')`,
		tag).Scan(&id, &level)
	return id, level, err
}

// CHECK-23: concurrent executed actions on a session without the pin each
// get an action_log row, and the chain stays whole. Before the explicit
// read committed transactions, contention failed them at COMMIT (23505).
func TestLoggedActionsUnderLostPinAreAllRecorded(t *testing.T) {
	e, pool, logs := unpinnedExecutor(t, "exec_iso_contention")
	ctx := context.Background()
	const workers, each = 6, 10
	var wg sync.WaitGroup
	var recorded atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				f := analyzer.Finding{Title: "iso", RecommendedSQL: fmt.Sprintf(
					"VACUUM iso_%d_%d", w, i)}
				if id := e.logClaimedAction(ctx, f, 0, nil, 0, nil, nil); id > 0 {
					recorded.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	if got := recorded.Load(); got != workers*each {
		t.Fatalf("recorded %d of %d executed actions; logs: %v", got, workers*each, *logs)
	}
	if n := actionLogCount(t, pool); n != workers*each {
		t.Fatalf("action_log rows = %d, want %d", n, workers*each)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.ActionLog, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != workers*each {
		t.Fatalf("chain: err=%v links=%d problems=%+v", err, rep.Links, rep.Problems)
	}
}

// When the claimed transaction fails, the action is still recorded, and
// both the claimed transaction and the fallback insert ran at read
// committed although the session default is repeatable read.
func TestRecordActionFallbackRunsReadCommitted(t *testing.T) {
	e, pool, logs := unpinnedExecutor(t, "exec_iso_fallback")
	e.recs = recommendation.NewStore(pool)
	ctx := context.Background()
	injected := errors.New("injected claimed-transaction failure")
	var levels []string
	id, err := e.recordAction(ctx, &recommendation.Claim{ID: 41}, nil,
		func(q actionLogWriter) (int64, error) {
			id, level, err := insertIsolated(ctx, q, "VACUUM fallback")
			levels = append(levels, level)
			if len(levels) == 1 && err == nil {
				return 0, injected // fail the claimed transaction after its insert
			}
			return id, err
		})
	if err != nil || id <= 0 {
		t.Fatalf("recordAction = %d, %v; want the fallback's id", id, err)
	}
	if len(levels) != 2 || levels[0] != "read committed" || levels[1] != "read committed" {
		t.Fatalf("insert isolation levels = %v, want [read committed read committed]", levels)
	}
	if n := actionLogCount(t, pool); n != 1 {
		t.Fatalf("action_log rows = %d, want exactly the fallback's 1", n)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "record recommendation 41 outcome") {
		t.Fatalf("the claimed transaction's failure was not logged: %v", *logs)
	}
}

// The unclaimed insert runs in its own read committed transaction too.
func TestRecordActionUnclaimedRunsReadCommitted(t *testing.T) {
	e, pool, _ := unpinnedExecutor(t, "exec_iso_unclaimed")
	ctx := context.Background()
	var level string
	id, err := e.recordAction(ctx, nil, nil, func(q actionLogWriter) (int64, error) {
		id, l, err := insertIsolated(ctx, q, "VACUUM unclaimed")
		level = l
		return id, err
	})
	if err != nil || id <= 0 || level != "read committed" {
		t.Fatalf("recordAction = %d, %v at %q; want an id at read committed", id, err, level)
	}
	if n := actionLogCount(t, pool); n != 1 {
		t.Fatalf("action_log rows = %d, want 1", n)
	}
}

// Error propagation: when the fallback fails too, the caller gets that
// error (not nil, not a zero id), and nothing half-written remains.
func TestRecordActionReturnsTheFallbackError(t *testing.T) {
	e, pool, _ := unpinnedExecutor(t, "exec_iso_bothfail")
	e.recs = recommendation.NewStore(pool)
	ctx := context.Background()
	fallbackErr := errors.New("fallback insert failed")
	calls := 0
	id, err := e.recordAction(ctx, &recommendation.Claim{ID: 7}, nil,
		func(actionLogWriter) (int64, error) {
			calls++
			if calls == 1 {
				return 0, errors.New("claimed transaction failed")
			}
			return 0, fallbackErr
		})
	if !errors.Is(err, fallbackErr) || id != 0 || calls != 2 {
		t.Fatalf("recordAction = %d, %v after %d calls; want 0, %v after 2",
			id, err, calls, fallbackErr)
	}
	if n := actionLogCount(t, pool); n != 0 {
		t.Fatalf("action_log rows = %d, want 0", n)
	}
}
