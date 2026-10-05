package ask

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// The registry maps a monitored database to its Ask Sage service; the
// API and MCP route every question through it. A database without a
// service (Ask disabled, or removed from the fleet) is "not found".

func TestRegistry_SetGetRemove(t *testing.T) {
	f := newFixture(t)
	r := NewRegistry()
	a, b := f.service(f.deps(nil)), f.service(f.deps(nil))
	r.Set("db1", a)
	if got, ok := r.Get("db1"); !ok || got != a {
		t.Fatalf("Get = %p %v", got, ok)
	}
	r.Remove("db1", b) // not the registered service: kept
	if got, _ := r.Get("db1"); got != a {
		t.Fatal("Remove of another service dropped the registered one")
	}
	r.Set("db1", b) // a restarted runtime replaces it
	r.Remove("db1", a)
	if got, _ := r.Get("db1"); got != b {
		t.Fatal("the stale runtime's Remove dropped its replacement")
	}
	r.Remove("db1", b)
	if _, ok := r.Get("db1"); ok {
		t.Fatal("removed service still registered")
	}
	r.Set("", a)
	r.Set("db2", nil)
	if _, ok := r.Get(""); ok {
		t.Fatal("empty name registered")
	}
	if _, ok := r.Get("db2"); ok {
		t.Fatal("nil service registered")
	}
}

func TestRegistry_AskUnknownDatabase(t *testing.T) {
	r := NewRegistry()
	_, err := r.AskSage(context.Background(), "nope", viewer, Request{Question: "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var nilRegistry *Registry
	if _, ok := nilRegistry.Get("db"); ok {
		t.Fatal("nil registry found a service")
	}
	if _, err := nilRegistry.AskSage(context.Background(), "db", viewer,
		Request{Question: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil registry: err = %v", err)
	}
}

func TestRegistry_RoutesToTheNamedDatabase(t *testing.T) {
	f := newFixture(t)
	m := newFakeLLM(t, answer(func(string) answerArgs {
		return answerArgs{NotObserved: []string{"Nothing."}}
	}))
	r := NewRegistry()
	d := f.deps(m)
	d.Database = "db1"
	r.Set("db1", f.service(d))
	a, err := r.AskSage(context.Background(), "db1", viewer, Request{Question: "Hi?"})
	if err != nil || a.Database != "db1" || a.Status != StatusNotObserved {
		t.Fatalf("answer = %+v (%v)", a, err)
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	f := newFixture(t)
	r := NewRegistry()
	s := f.service(f.deps(nil))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.Set("db", s) }()
		go func() { defer wg.Done(); _, _ = r.Get("db") }()
	}
	wg.Wait()
	if got, ok := r.Get("db"); !ok || got != s {
		t.Fatal("registry lost the service under concurrent use")
	}
	if names := r.Databases(); len(names) != 1 || names[0] != "db" {
		t.Fatalf("databases = %v", names)
	}
}
