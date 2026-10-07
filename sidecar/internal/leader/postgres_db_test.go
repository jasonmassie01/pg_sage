package leader

import (
	"context"
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
	pool, err := pgxpool.New(ctx, os.Getenv(testdb.EnvName))
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

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
