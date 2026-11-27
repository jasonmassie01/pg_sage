package migration

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// No integration test: the cache is in-memory and reads no database.

func (c *replyCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func TestReplyCacheTTLBoundary(t *testing.T) {
	c := newReplyCache(4, 24*time.Hour)
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	now := t0
	c.now = func() time.Time { return now }
	c.put("k", "reply")
	now = t0.Add(24*time.Hour - time.Nanosecond)
	if got, ok := c.get("k"); !ok || got != "reply" {
		t.Fatalf("get just inside the TTL = %q, %v; want the reply", got, ok)
	}
	now = t0.Add(24 * time.Hour)
	if got, ok := c.get("k"); ok {
		t.Fatalf("get at the TTL = %q, want a miss", got)
	}
	if c.len() != 0 {
		t.Fatalf("expired entry kept: len = %d", c.len())
	}
}

func TestReplyCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newReplyCache(enrichCacheSize, enrichCacheTTL)
	for i := 0; i < enrichCacheSize; i++ {
		c.put(fmt.Sprint("k", i), fmt.Sprint("r", i))
	}
	if _, ok := c.get("k0"); !ok {
		t.Fatal("k0 missing before the cache is full")
	}
	c.put("new", "r-new")
	if c.len() != enrichCacheSize {
		t.Fatalf("len = %d, want %d", c.len(), enrichCacheSize)
	}
	if _, ok := c.get("k1"); ok {
		t.Fatal("k1, the least recently used entry, was not evicted")
	}
	for _, k := range []string{"k0", "k2", "new"} {
		if _, ok := c.get(k); !ok {
			t.Errorf("%s evicted, want it kept", k)
		}
	}
	c.put("k0", "r0-updated")
	if got, _ := c.get("k0"); got != "r0-updated" {
		t.Errorf("updated k0 = %q", got)
	}
	if c.len() != enrichCacheSize {
		t.Errorf("an update grew the cache to %d", c.len())
	}
}

func TestReplyCacheNilAndEmpty(t *testing.T) {
	var c *replyCache
	c.put("k", "v")
	if _, ok := c.get("k"); ok {
		t.Fatal("nil cache returned a hit")
	}
	e := newReplyCache(2, time.Hour)
	if _, ok := e.get(""); ok {
		t.Fatal("empty cache returned a hit")
	}
}

func TestReplyCacheConcurrentAccess(t *testing.T) {
	c := newReplyCache(8, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := fmt.Sprint("k", i%12)
			c.put(k, k)
			if got, ok := c.get(k); ok && got != k {
				t.Errorf("get(%s) = %q", k, got)
			}
		}(i)
	}
	wg.Wait()
	if c.len() > 8 {
		t.Fatalf("len = %d, want at most 8", c.len())
	}
}

func TestEnrichKeyIdentity(t *testing.T) {
	base := enrichKey("ddl_vacuum_full", "VACUUM FULL orders", "-- Table: public.orders")
	if len(base) != 64 {
		t.Fatalf("key %q is not a hex sha256", base)
	}
	same := []string{
		enrichKey("ddl_vacuum_full", "  vacuum   FULL\n orders ", "-- Table: public.orders"),
	}
	for _, k := range same {
		if k != base {
			t.Errorf("whitespace or case changed the key")
		}
	}
	lit := enrichKey("r", "ALTER TABLE t ALTER c SET DEFAULT 'a'", "")
	if lit != enrichKey("r", "ALTER TABLE t ALTER c SET DEFAULT 'b'", "") {
		t.Error("a redacted literal changed the key")
	}
	differ := map[string]string{
		"rule":      enrichKey("ddl_cluster", "VACUUM FULL orders", "-- Table: public.orders"),
		"statement": enrichKey("ddl_vacuum_full", "VACUUM FULL items", "-- Table: public.orders"),
		"schema": enrichKey("ddl_vacuum_full", "VACUUM FULL orders",
			"-- Table: public.orders\n--   id int"),
	}
	for what, k := range differ {
		if k == base {
			t.Errorf("a different %s gave the same key", what)
		}
	}
	// The separator keeps field boundaries: ("ab","c") is not ("a","bc").
	if enrichKey("ab", "c", "") == enrichKey("a", "bc", "") {
		t.Error("field boundaries collide")
	}
}
