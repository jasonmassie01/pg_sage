package testdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// ci-16 without a server: the shared pool registry's input checks, its
// one-pool-per-DSN rule, its idle/open/closed states and concurrent use.
// Creating a pool does not connect, so none of these dial.

const (
	poolDSNA = "postgres://u:p@127.0.0.1:1/a?sslmode=disable&connect_timeout=1"
	poolDSNB = "postgres://u:p@127.0.0.1:1/b?sslmode=disable&connect_timeout=1"
)

func openRegistry(t *testing.T) *poolRegistry {
	t.Helper()
	r := &poolRegistry{}
	r.open()
	t.Cleanup(r.close)
	return r
}

// Nil/empty: an empty DSN is "no DSN", never a pool to an implicit localhost.
func TestPoolRegistry_EmptyDSNIsNoDSN(t *testing.T) {
	r := openRegistry(t)
	for _, dsn := range []string{"", "   ", "\t\n"} {
		pool, err := r.get(dsn)
		if pool != nil || !errors.Is(err, errNoDSN) ||
			!strings.Contains(err.Error(), "no DSN") {
			t.Fatalf("get(%q) = %v, %v; want nil and the no-DSN error", dsn, pool, err)
		}
	}
	if len(r.pools) != 0 {
		t.Fatalf("an empty DSN left %d pools", len(r.pools))
	}
}

// Invalid input: a malformed DSN fails with pgx's parse error, wrapped.
func TestPoolRegistry_MalformedDSNIsTheParseError(t *testing.T) {
	r := openRegistry(t)
	for _, dsn := range []string{
		"postgres://u@127.0.0.1:notaport/db",
		"postgres://u@127.0.0.1/db?pool_max_conns=many",
	} {
		pool, err := r.get(dsn)
		if pool != nil || err == nil ||
			!strings.Contains(err.Error(), "parse the shared pool's DSN") {
			t.Fatalf("get(%q) = %v, %v; want nil and the parse error", dsn, pool, err)
		}
		if errors.Is(err, errNoDSN) {
			t.Fatalf("a malformed DSN must not read as missing: %v", err)
		}
	}
	if len(r.pools) != 0 {
		t.Fatalf("malformed DSNs left %d pools", len(r.pools))
	}
}

// Happy path: one pool per DSN, the same one on every call.
func TestPoolRegistry_OnePoolPerDSN(t *testing.T) {
	r := openRegistry(t)
	a1, err := r.get(poolDSNA)
	if err != nil || a1 == nil {
		t.Fatalf("get(a) = %v, %v", a1, err)
	}
	a2, _ := r.get(poolDSNA)
	b, err := r.get(poolDSNB)
	if err != nil || b == nil {
		t.Fatalf("get(b) = %v, %v", b, err)
	}
	if a1 != a2 {
		t.Fatal("the same DSN returned two pools")
	}
	if a1 == b {
		t.Fatal("two DSNs share one pool")
	}
	if got := b.Config().ConnConfig.Database; got != "b" {
		t.Fatalf("pool for DSN b connects to %q", got)
	}
	if len(r.pools) != 2 {
		t.Fatalf("%d pools for two DSNs", len(r.pools))
	}
}

// Boundary: MaxConns is 8 unless the DSN names pool_max_conns.
func TestPoolRegistry_MaxConnsDefaultsToEight(t *testing.T) {
	r := openRegistry(t)
	a, _ := r.get(poolDSNA)
	if a == nil || a.Config().MaxConns != SharedPoolMaxConns || SharedPoolMaxConns != 8 {
		t.Fatalf("MaxConns %v, want 8", a)
	}
	for _, n := range []string{"1", "20"} {
		p, err := r.get(poolDSNA + "&pool_max_conns=" + n)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Config().MaxConns; got != map[string]int32{"1": 1, "20": 20}[n] {
			t.Fatalf("pool_max_conns=%s gave MaxConns %d", n, got)
		}
	}
}

// State transitions: idle refuses, open serves, closed refuses and closes
// every pool, and a new run opens fresh pools.
func TestPoolRegistry_StatesAndClose(t *testing.T) {
	r := &poolRegistry{}
	if _, err := r.get(poolDSNA); !errors.Is(err, errPoolsNotOpen) {
		t.Fatalf("idle registry: %v, want the not-open error", err)
	}
	r.open()
	a, err := r.get(poolDSNA)
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	if _, err := r.get(poolDSNA); !errors.Is(err, errPoolsClosed) {
		t.Fatalf("closed registry: %v, want the closed error", err)
	}
	if _, err := a.Acquire(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "closed pool") {
		t.Fatalf("Acquire on a pool closed by the registry: %v", err)
	}
	r.open()
	t.Cleanup(r.close)
	again, err := r.get(poolDSNA)
	if err != nil || again == a {
		t.Fatalf("reopened registry returned %v (old %v), %v; want a new pool", again, a, err)
	}
	r.close()
	r.close() // closing twice is safe
}

// Error propagation: PoolFor fails the test with the registry's reason.
func TestPoolFor_FailsTheTestWithTheReason(t *testing.T) {
	saved := sharedPools
	t.Cleanup(func() { sharedPools = saved })
	sharedPools = &poolRegistry{}
	rec := &recordingTB{TB: t}
	if p := PoolFor(rec, poolDSNA); p != nil || !strings.Contains(rec.fatal, "not open") {
		t.Fatalf("idle: pool %v, fatal %q", p, rec.fatal)
	}
	sharedPools = openRegistry(t)
	rec = &recordingTB{TB: t}
	if p := PoolFor(rec, ""); p != nil || !strings.Contains(rec.fatal, "no DSN") {
		t.Fatalf("empty DSN: pool %v, fatal %q", p, rec.fatal)
	}
	rec = &recordingTB{TB: t}
	if p := PoolFor(rec, poolDSNA); p == nil || rec.fatal != "" {
		t.Fatalf("open: pool %v, fatal %q", p, rec.fatal)
	}
}

// Concurrent access: 50 goroutines asking for one DSN get one pool.
func TestPoolRegistry_ConcurrentGetsShareOnePool(t *testing.T) {
	r := openRegistry(t)
	const n = 50
	got := make([]any, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := r.get(poolDSNA)
			if err != nil {
				got[i] = err
				return
			}
			got[i] = p
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Fatalf("goroutine %d got %v, goroutine 0 got %v", i, got[i], got[0])
		}
	}
	if len(r.pools) != 1 {
		t.Fatalf("%d pools for one DSN", len(r.pools))
	}
}
