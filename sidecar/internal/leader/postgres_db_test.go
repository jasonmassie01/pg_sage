package leader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/leader"))
}

var (
	bootOnce sync.Once
	bootErr  error
)

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Skipf("no test PostgreSQL (%s): %v", testdb.EnvName, err)
	}
	t.Cleanup(pool.Close)
	bootOnce.Do(func() { bootErr = schema.Bootstrap(context.Background(), pool) })
	if bootErr != nil {
		t.Fatalf("bootstrap: %v", bootErr)
	}
	return pool
}

func uniqueScope(t *testing.T) string {
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

func TestPostgresStore_AcquireRenewAndConflict(t *testing.T) {
	ctx := context.Background()
	st := NewPostgresStore(dbPool(t))
	scope := uniqueScope(t)
	l, held, err := st.Acquire(ctx, scope, "A", time.Minute)
	if err != nil || !held || l.Holder != "A" || l.Epoch != 1 {
		t.Fatalf("first acquire = %+v held=%v err=%v", l, held, err)
	}
	if !l.ExpiresAt.After(time.Now().Add(50 * time.Second)) {
		t.Fatalf("expires_at = %v, want about a minute ahead", l.ExpiresAt)
	}
	l2, held, err := st.Acquire(ctx, scope, "A", time.Minute)
	if err != nil || !held || l2.Epoch != 1 {
		t.Fatalf("renewal = %+v held=%v err=%v, want same epoch", l2, held, err)
	}
	cur, held, err := st.Acquire(ctx, scope, "B", time.Minute)
	if err != nil || held || cur.Holder != "A" {
		t.Fatalf("contender = %+v held=%v err=%v, want refused with holder A",
			cur, held, err)
	}
}

func TestPostgresStore_TakeoverAfterExpiryBumpsEpoch(t *testing.T) {
	ctx := context.Background()
	pool := dbPool(t)
	st := NewPostgresStore(pool)
	scope := uniqueScope(t)
	if _, _, err := st.Acquire(ctx, scope, "A", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.fleet_leader_lease
		SET expires_at = now() - interval '1 second' WHERE scope=$1`, scope); err != nil {
		t.Fatalf("expire: %v", err)
	}
	l, held, err := st.Acquire(ctx, scope, "B", time.Minute)
	if err != nil || !held || l.Holder != "B" || l.Epoch != 2 {
		t.Fatalf("takeover = %+v held=%v err=%v, want B epoch 2", l, held, err)
	}
	// The old holder comes back: it must not steal the lease back.
	if _, held, _ := st.Acquire(ctx, scope, "A", time.Minute); held {
		t.Fatal("a stale holder re-acquired a live lease")
	}
}

func TestPostgresStore_ReleaseOnlyByHolder(t *testing.T) {
	ctx := context.Background()
	st := NewPostgresStore(dbPool(t))
	scope := uniqueScope(t)
	_, _, _ = st.Acquire(ctx, scope, "A", time.Minute)
	if err := st.Release(ctx, scope, "B"); err != nil {
		t.Fatalf("foreign release: %v", err)
	}
	if _, held, _ := st.Acquire(ctx, scope, "B", time.Minute); held {
		t.Fatal("a non-holder released someone else's lease")
	}
	if err := st.Release(ctx, scope, "A"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if l, held, _ := st.Acquire(ctx, scope, "B", time.Minute); !held || l.Epoch != 2 {
		t.Fatalf("after release B = %+v held=%v", l, held)
	}
}

// TestPostgresStore_NoSplitBrainUnderContention races many contenders for
// an expired lease, many times: exactly one may win each round.
func TestPostgresStore_NoSplitBrainUnderContention(t *testing.T) {
	ctx := context.Background()
	pool := dbPool(t)
	st := NewPostgresStore(pool)
	for round := 0; round < 10; round++ {
		scope := fmt.Sprintf("%s-%d", uniqueScope(t), round)
		_, _, _ = st.Acquire(ctx, scope, "old", time.Minute)
		_, _ = pool.Exec(ctx, `UPDATE sage.fleet_leader_lease
			SET expires_at = now() - interval '1 second' WHERE scope=$1`, scope)
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := []string{}
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(holder string) {
				defer wg.Done()
				<-start
				l, held, err := st.Acquire(ctx, scope, holder, time.Minute)
				if err != nil {
					t.Errorf("acquire %s: %v", holder, err)
					return
				}
				if held {
					mu.Lock()
					winners = append(winners, l.Holder)
					mu.Unlock()
				}
			}(fmt.Sprintf("c%d", i))
		}
		close(start)
		wg.Wait()
		if len(winners) != 1 {
			t.Fatalf("round %d: %d winners %v, want exactly 1", round, len(winners), winners)
		}
	}
}

func TestPostgresStore_ElectorsAgainstRealDatabase(t *testing.T) {
	ctx := context.Background()
	st := NewPostgresStore(dbPool(t))
	scope := uniqueScope(t)
	a := NewElector(st, scope, "A", 2*time.Second)
	b := NewElector(st, scope, "B", 2*time.Second)
	if err := a.Tick(ctx); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := b.Tick(ctx); err != nil {
		t.Fatalf("b: %v", err)
	}
	if !a.IsLeader() || b.IsLeader() {
		t.Fatalf("a=%v b=%v, want only A", a.IsLeader(), b.IsLeader())
	}
	time.Sleep(2100 * time.Millisecond)
	if a.IsLeader() {
		t.Fatal("A leads past its lease without renewing")
	}
	if err := b.Tick(ctx); err != nil {
		t.Fatalf("b takeover: %v", err)
	}
	if !b.IsLeader() {
		t.Fatal("B did not take over the expired lease")
	}
	cur, ok, err := st.Current(ctx, scope)
	if err != nil || !ok || cur.Holder != "B" || cur.Epoch != 2 {
		t.Fatalf("current = %+v ok=%v err=%v", cur, ok, err)
	}
	if _, ok, err := st.Current(ctx, "never-"+scope); ok || err != nil {
		t.Fatalf("unknown scope current ok=%v err=%v", ok, err)
	}
}

func TestPostgresStore_ErrorsNameTheOperation(t *testing.T) {
	st := NewPostgresStore(nil)
	if _, _, err := st.Acquire(context.Background(), "s", "h", time.Second); err == nil {
		t.Fatal("nil pool must fail")
	}
	pool := dbPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := NewPostgresStore(pool).Acquire(ctx, "s", "h", time.Second)
	if err == nil || !containsAll(err.Error(), "acquire", "lease") {
		t.Fatalf("cancelled acquire err = %v", err)
	}
}

// TestPostgresStore_BlockedRenewalTimesOutAtTickBound holds the lease row
// lock in another transaction (pg_sleep keeps it busy): the elector's
// renewal must give up within ttl/3 with context.DeadlineExceeded, leave no
// waiting backend behind, and renew once the lock is gone.
func TestPostgresStore_BlockedRenewalTimesOutAtTickBound(t *testing.T) {
	ctx := context.Background()
	pool := dbPool(t)
	st := NewPostgresStore(pool)
	scope := uniqueScope(t)
	ttl := 900 * time.Millisecond
	e := NewElector(st, scope, "A", ttl)
	if err := e.Tick(ctx); err != nil || !e.IsLeader() {
		t.Fatalf("first tick err=%v leader=%v", err, e.IsLeader())
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sage.fleet_leader_lease
		WHERE scope = $1 FOR UPDATE`, scope); err != nil {
		t.Fatalf("lock lease row: %v", err)
	}
	sleepDone := make(chan error, 1)
	go func() { _, err := tx.Exec(ctx, `SELECT pg_sleep(1.5)`); sleepDone <- err }()
	start := time.Now()
	err = e.Tick(ctx)
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked renewal err = %v, want context.DeadlineExceeded", err)
	}
	if bound := ttl/3 + 250*time.Millisecond; took > bound {
		t.Fatalf("blocked renewal took %v, want within %v", took, bound)
	}
	if n := waitingRenewals(t, pool, scope); n != 0 {
		t.Fatalf("%d renewal backends still waiting on the row lock", n)
	}
	if err := <-sleepDone; err != nil {
		t.Fatalf("pg_sleep holder: %v", err)
	}
	_ = tx.Rollback(ctx)
	if err := e.Tick(ctx); err != nil {
		t.Fatalf("renewal after the lock cleared: %v", err)
	}
	if s := e.Status(); !s.Leader || s.LastError != "" {
		t.Fatalf("status after recovery = %+v, want leader with no error", s)
	}
}

// waitingRenewals counts lease UPSERTs on scope still waiting on a lock,
// polling briefly because cancellation reaches the server asynchronously.
func waitingRenewals(t *testing.T, pool *pgxpool.Pool, scope string) int {
	t.Helper()
	var n int
	for i := 0; i < 40; i++ {
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%fleet_leader_lease AS l%'
			AND pid <> pg_backend_pid()`).Scan(&n)
		if err != nil {
			t.Fatalf("read pg_stat_activity for %s: %v", scope, err)
		}
		if n == 0 {
			return 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
