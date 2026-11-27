package clusterlock_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clusterlock"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// benches-01: the cluster lock leaves internal/testdb so the sidecar can
// hold a bench target across processes. TryLock never waits; the lock is
// session-scoped, so it ends with its connection.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/clusterlock"))
}

func uniqueName(t *testing.T) string {
	t.Helper()
	return "test_" + strings.ReplaceAll(t.Name(), "/", "_") + "_" +
		time.Now().Format("150405.000000000")
}

func tryLock(t *testing.T, dsn, name string) (func(), bool) {
	t.Helper()
	release, ok, err := clusterlock.TryLock(context.Background(), dsn, name)
	if err != nil {
		t.Fatalf("TryLock(%q): %v", name, err)
	}
	if ok != (release != nil) {
		t.Fatalf("TryLock(%q) = ok %v with release %v", name, ok, release != nil)
	}
	return release, ok
}

func TestTryLockIsExclusiveUntilReleased(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	name := uniqueName(t)
	release, ok := tryLock(t, dsn, name)
	if !ok {
		t.Fatal("a free lock was refused")
	}
	if again, ok := tryLock(t, dsn, name); ok {
		again()
		t.Fatal("a held lock was granted to a second session")
	}
	other, ok := tryLock(t, dsn, name+"_other")
	if !ok {
		t.Fatal("another name was refused while this one is held")
	}
	other()
	release()
	release() // releasing twice is harmless
	after, ok := tryLock(t, dsn, name)
	if !ok {
		t.Fatal("a released lock was refused")
	}
	after()
}

// The lock is taken in the maintenance database, so two databases of one
// cluster share it (a fault program acts on the whole cluster).
func TestTryLockIsClusterWide(t *testing.T) {
	a := testdb.CreateDatabase(t, "clusterlock_a")
	b := testdb.CreateDatabase(t, "clusterlock_b")
	name := uniqueName(t)
	release, ok := tryLock(t, a, name)
	if !ok {
		t.Fatal("a free lock was refused")
	}
	defer release()
	if other, ok := tryLock(t, b, name); ok {
		other()
		t.Fatal("a sibling database took the cluster lock while it is held")
	}
}

// Lock waits for the holder; a context that ends first is an error.
func TestLockWaitsForTheHolder(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	name := uniqueName(t)
	release, ok := tryLock(t, dsn, name)
	if !ok {
		t.Fatal("a free lock was refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if r, err := clusterlock.Lock(ctx, dsn, name); err == nil {
		r()
		t.Fatal("Lock returned while another session holds the lock")
	} else if !strings.Contains(err.Error(), name) {
		t.Fatalf("Lock error does not name the lock: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := clusterlock.Lock(context.Background(), dsn, name)
		if err == nil {
			r()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("Lock returned before the holder released: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Lock after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lock never acquired the released lock")
	}
}

// Many sessions racing for one free lock: exactly one wins.
func TestTryLockConcurrentCallersGetOneHolder(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	name := uniqueName(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	releases := make(chan func(), 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, ok, err := clusterlock.TryLock(context.Background(), dsn, name)
			switch {
			case err != nil:
				errs <- err
			case ok:
				wins.Add(1)
				releases <- release
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(releases)
	for err := range errs {
		t.Errorf("TryLock: %v", err)
	}
	for r := range releases {
		r()
	}
	if wins.Load() != 1 {
		t.Fatalf("%d sessions hold one lock, want exactly 1", wins.Load())
	}
}

// Invalid input fails with an error that says what was attempted.
func TestTryLockInvalidInput(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ dsn, name, want string }{
		{"postgres://u:p@127.0.0.1:1/db?connect_timeout=1", "", "lock name"},
		{"not a dsn ::: %", "x", "parse dsn"},
		{"postgres://u:p@127.0.0.1:1/db?connect_timeout=1", "x", "maintenance database"},
	}
	for _, c := range cases {
		release, ok, err := clusterlock.TryLock(ctx, c.dsn, c.name)
		if err == nil || ok || release != nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("TryLock(%q, %q) = %v %v, want an error naming %q", c.dsn, c.name,
				ok, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), ":p@") {
			t.Errorf("the error leaks the DSN password: %v", err)
		}
		r, err := clusterlock.Lock(ctx, c.dsn, c.name)
		if err == nil || r != nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Lock(%q, %q) = %v, want an error naming %q", c.dsn, c.name, err,
				c.want)
		}
	}
}

// testdb re-exports the lock for test fixtures: one lock, one namespace.
func TestTestdbLockClusterIsTheSameLock(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	name := uniqueName(t)
	release, err := testdb.LockCluster(context.Background(), dsn, name)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if other, ok := tryLock(t, dsn, name); ok {
		other()
		t.Fatal("TryLock was granted a lock testdb.LockCluster holds")
	}
}
