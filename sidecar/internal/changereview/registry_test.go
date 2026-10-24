package changereview

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The process-wide registry maps each database to its service; a runtime
// that stops removes only its own registration.

func TestRegistrySetGetRemove(t *testing.T) {
	r := NewRegistry()
	a, b := &Service{}, &Service{}
	r.Set("db1", a)
	r.Set("db2", b)
	r.Set("", a)
	r.Set("db3", nil)
	if got := r.Databases(); len(got) != 2 || got[0] != "db1" || got[1] != "db2" {
		t.Fatalf("databases = %v", got)
	}
	if s, ok := r.Get("db1"); !ok || s != a {
		t.Fatal("db1 not found")
	}
	newer := &Service{}
	r.Set("db1", newer)
	r.Remove("db1", a)
	if s, _ := r.Get("db1"); s != newer {
		t.Fatal("removing the old runtime unregistered the newer one")
	}
	r.Remove("db1", newer)
	if _, ok := r.Get("db1"); ok {
		t.Fatal("db1 still registered")
	}
	if _, err := r.Service("db1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Service(db1) = %v", err)
	}
	var nilReg *Registry
	nilReg.Set("x", a)
	nilReg.Remove("x", a)
	if _, ok := nilReg.Get("x"); ok || nilReg.Databases() != nil {
		t.Fatal("a nil registry is not empty")
	}
	if _, err := nilReg.Service("x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a nil registry finds services")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &Service{}
			name := []string{"a", "b", "c"}[i%3]
			r.Set(name, s)
			r.Get(name)
			r.Databases()
			r.Remove(name, s)
		}(i)
	}
	wg.Wait()
	if n := len(r.Databases()); n > 3 {
		t.Fatalf("%d databases after the race", n)
	}
}

func TestRegistryFleetSummary(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(staticAnalysis(t)))
	submitAndWait(t, f, s, sqlRequest("ALTER TABLE t ADD COLUMN z int"))
	r := NewRegistry()
	r.Set("testdb", s)
	brokenPool, err := pgxpool.New(f.ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	d := f.deps(staticAnalysis(t))
	d.Database, d.Pool = "down", brokenPool
	r.Set("down", f.service(d))
	brokenPool.Close()
	sum, err := r.Summary(f.ctx, "", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("fleet summary: %v", err)
	}
	if sum.Reviews != 1 || !sum.Partial || len(sum.Unavailable) != 1 ||
		sum.Unavailable[0] != "down" {
		t.Fatalf("fleet summary = %+v", sum)
	}
	one, err := r.Summary(f.ctx, "testdb", time.Now().Add(-time.Hour))
	if err != nil || one.Reviews != 1 || one.Partial {
		t.Fatalf("one database = %+v %v", one, err)
	}
	if _, err := r.Summary(f.ctx, "nope", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown database = %v", err)
	}
}
