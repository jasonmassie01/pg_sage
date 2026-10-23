package decide

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard/decide"))
}

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

var seq atomic.Int64

func livePrincipal(t *testing.T, pool *pgxpool.Pool, status agentguard.Status) string {
	t.Helper()
	store := agentguard.NewStore(pool)
	p, err := store.Create(context.Background(), agentguard.CreateRequest{
		Name:    fmt.Sprintf("hold-%d-%d", time.Now().UnixNano()%1e9, seq.Add(1)),
		Profile: "readonly-analyst", EnvCeiling: agentguard.EnvProd,
		CreatedBy: "admin@example.com"})
	if err != nil {
		t.Fatalf("create principal: %v", err)
	}
	if status != agentguard.StatusActive {
		if _, err := store.SetStatus(context.Background(), p.ID, status,
			"test"); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	return p.ID
}

// killLock is the kill's side of §6.2.7: FOR UPDATE on the principal with
// a lock_timeout.
func killLock(ctx context.Context, pool *pgxpool.Pool, id string, timeout string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)",
		timeout); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT 1 FROM sage.guard_principals WHERE id = $1 FOR UPDATE`, id)
	return err
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

func TestHoldActiveBlocksTheKillUntilReleased(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	id := livePrincipal(t, pool, agentguard.StatusActive)
	release, err := HoldActive(ctx, pool, id)
	if err != nil {
		t.Fatalf("HoldActive: %v", err)
	}
	if err := killLock(ctx, pool, id, "200ms"); !isLockTimeout(err) {
		t.Fatalf("kill lock while held = %v, want lock_timeout (55P03)", err)
	}
	release()
	release() // idempotent
	if err := killLock(ctx, pool, id, "2s"); err != nil {
		t.Fatalf("kill lock after release = %v", err)
	}
}

func TestHoldActiveKillWaitsThenProceeds(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	id := livePrincipal(t, pool, agentguard.StatusActive)
	release, err := HoldActive(ctx, pool, id)
	if err != nil {
		t.Fatalf("HoldActive: %v", err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- killLock(ctx, pool, id, "5s") }()
	time.Sleep(300 * time.Millisecond)
	release()
	if err := <-done; err != nil {
		t.Fatalf("kill after the commit = %v", err)
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("kill took %v: it did not wait for the in-flight write", waited)
	}
}

func TestHoldActiveSharedHoldsCoexist(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	id := livePrincipal(t, pool, agentguard.StatusActive)
	r1, err := HoldActive(ctx, pool, id)
	if err != nil {
		t.Fatalf("first hold: %v", err)
	}
	defer r1()
	r2, err := HoldActive(ctx, pool, id)
	if err != nil {
		t.Fatalf("second concurrent write of the same agent: %v", err)
	}
	r2()
}

func TestHoldActiveRefusesInactivePrincipals(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	cases := map[agentguard.Status]agentguard.Reason{
		agentguard.StatusFrozen:  agentguard.ReasonFrozen,
		agentguard.StatusRetired: agentguard.ReasonRetired,
	}
	for status, reason := range cases {
		id := livePrincipal(t, pool, status)
		release, err := HoldActive(ctx, pool, id)
		denied, ok := agentguard.IsDenied(err)
		if release != nil || !ok || denied.Reason != reason {
			t.Fatalf("%s: HoldActive = %v, want denied %s", status, err, reason)
		}
	}
	_, err := HoldActive(ctx, pool, "agp_zzzzzzzzzzzzzzzzzzzz")
	if denied, ok := agentguard.IsDenied(err); !ok || denied.Reason != agentguard.ReasonRetired {
		t.Fatalf("unknown principal = %v, want denied agent_retired", err)
	}
}

func TestHoldActiveFailsClosedWithoutControl(t *testing.T) {
	if _, err := HoldActive(context.Background(), nil, testID); !errors.Is(err,
		agentguard.ErrUnavailable) {
		t.Fatalf("nil pool = %v, want ErrUnavailable", err)
	}
	if _, err := HoldActive(context.Background(), nil, ""); !errors.Is(err,
		agentguard.ErrInvalid) {
		t.Fatalf("empty id = %v, want ErrInvalid", err)
	}
}

func TestHoldActiveCancelledContext(t *testing.T) {
	pool := livePool(t)
	id := livePrincipal(t, pool, agentguard.StatusActive)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HoldActive(ctx, pool, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v, want context.Canceled", err)
	}
}
