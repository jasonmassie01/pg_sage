package pgaudit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRoles is a role loader that counts its reads and fails on demand.
type fakeRoles struct {
	reads atomic.Int32
	mu    sync.Mutex
	rows  []roleRow
	err   error
	delay time.Duration
}

func (f *fakeRoles) load(context.Context) ([]roleRow, error) {
	f.reads.Add(1)
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.err
}

func (f *fakeRoles) set(rows []roleRow, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestCache(f *fakeRoles, warns *[]error) (*RoleCache, *clock) {
	clk := &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	var onError func(error)
	if warns != nil {
		onError = func(err error) { *warns = append(*warns, err) }
	}
	return newRoleCache(f.load, onError, clk.now), clk
}

// Boundary: the control database is read once per roleCacheTTL (60 s), not
// once per batch; the read at exactly the TTL refreshes.
func TestRoleCacheReadsOncePerTTL(t *testing.T) {
	f := &fakeRoles{rows: []roleRow{ownerRow(idA, nameA)}}
	c, clk := newTestCache(f, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		m, err := c.Roles(ctx)
		if err != nil || len(m) != 2 {
			t.Fatalf("read %d: %v %v", i, m, err)
		}
	}
	clk.t = clk.t.Add(roleCacheTTL - time.Millisecond)
	if _, err := c.Roles(ctx); err != nil || f.reads.Load() != 1 {
		t.Fatalf("within the TTL: %d reads, %v", f.reads.Load(), err)
	}
	f.set([]roleRow{ownerRow(idA, nameA), ownerRow(idB, nameB)}, nil)
	clk.t = clk.t.Add(time.Millisecond)
	m, err := c.Roles(ctx)
	if err != nil || f.reads.Load() != 2 || len(m) != 4 {
		t.Fatalf("at the TTL: %d reads, %d roles, %v", f.reads.Load(), len(m), err)
	}
	if roleCacheTTL != 60*time.Second {
		t.Fatalf("roleCacheTTL = %v, want 60s", roleCacheTTL)
	}
}

// Error propagation: a failed read returns no roles (never the last good
// map), is retried on the next call, and warns once per failing streak.
func TestRoleCacheFailureWarnsOncePerStreak(t *testing.T) {
	denied := errors.New("permission denied for table guard_cluster_roles")
	f := &fakeRoles{err: denied}
	var warns []error
	c, clk := newTestCache(f, &warns)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		m, err := c.Roles(ctx)
		if m != nil || !errors.Is(err, denied) {
			t.Fatalf("failing read %d = %v, %v", i, m, err)
		}
	}
	if f.reads.Load() != 3 || len(warns) != 1 || !errors.Is(warns[0], denied) {
		t.Fatalf("streak 1: %d reads, warns %v", f.reads.Load(), warns)
	}
	f.set([]roleRow{ownerRow(idA, nameA)}, nil)
	if m, err := c.Roles(ctx); err != nil || len(m) != 2 {
		t.Fatalf("recovered read = %v, %v", m, err)
	}
	f.set(nil, denied)
	clk.t = clk.t.Add(roleCacheTTL)
	if m, err := c.Roles(ctx); m != nil || err == nil {
		t.Fatalf("failure after a good read served %v, %v", m, err)
	}
	if len(warns) != 2 {
		t.Fatalf("a new streak warns again: %d warns", len(warns))
	}
}

// Nil/empty: a nil cache, or a source with no control database, resolves
// nothing and is not an error; a nil onError never panics.
func TestRoleCacheNilAndEmpty(t *testing.T) {
	ctx := context.Background()
	var none *RoleCache
	if m, err := none.Roles(ctx); m != nil || err != nil {
		t.Fatalf("nil cache = %v, %v", m, err)
	}
	if m, err := NewRoleCache(nil, nil).Roles(ctx); len(m) != 0 || err != nil {
		t.Fatalf("nil source = %v, %v", m, err)
	}
	noControl := NewRoleCache(func() RoleQuerier { return nil }, func(error) {
		t.Fatalf("no control database is not a failure")
	})
	if m, err := noControl.Roles(ctx); len(m) != 0 || err != nil {
		t.Fatalf("no control database = %v, %v", m, err)
	}
	f := &fakeRoles{err: errors.New("boom")}
	c, _ := newTestCache(f, nil)
	if _, err := c.Roles(ctx); err == nil {
		t.Fatalf("a failing read with no onError returned no error")
	}
}

// Concurrent access: batches of several databases racing on one cache read
// the control database once and see the same roles.
func TestRoleCacheConcurrentReaders(t *testing.T) {
	f := &fakeRoles{rows: []roleRow{ownerRow(idA, nameA), ownerRow(idB, nameB)},
		delay: 20 * time.Millisecond}
	c, _ := newTestCache(f, nil)
	const workers = 16
	var wg sync.WaitGroup
	sizes := make(chan int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := c.Roles(context.Background())
			if err != nil {
				sizes <- -1
				return
			}
			o, _ := m.PrincipalOfRole(ownerRow(idB, nameB).brokerRole)
			if o.ID != idB {
				sizes <- -2
				return
			}
			sizes <- len(m)
		}()
	}
	wg.Wait()
	close(sizes)
	for n := range sizes {
		if n != 4 {
			t.Fatalf("a concurrent reader saw %d", n)
		}
	}
	if f.reads.Load() != 1 {
		t.Fatalf("%d reads of the control database, want 1", f.reads.Load())
	}
}
