package earned

import (
	"fmt"
	"sync"
	"testing"
)

// The registry maps fleet database names to their ledger (shared when
// the databases share a control database) and their per-database limiter.

func TestRegistryRegistersLooksUpAndRemoves(t *testing.T) {
	r := NewRegistry(true)
	if !r.Enforced() || len(r.Databases()) != 0 {
		t.Fatalf("new registry: enforced=%v dbs=%v", r.Enforced(), r.Databases())
	}
	svc := &Service{}
	r.Register("orders", RegistryEntry{Service: svc})
	r.Register("billing", RegistryEntry{Service: svc})
	if got := r.Databases(); len(got) != 2 || got[0] != "billing" || got[1] != "orders" {
		t.Fatalf("databases = %v, want sorted", got)
	}
	e, ok := r.Lookup("orders")
	if !ok || e.Service != svc {
		t.Fatalf("lookup = %+v %v", e, ok)
	}
	r.Remove("orders")
	if _, ok := r.Lookup("orders"); ok {
		t.Fatal("removed database still resolves")
	}
	if _, ok := r.Lookup(""); ok {
		t.Fatal("empty name resolves")
	}
	if NewRegistry(false).Enforced() {
		t.Fatal("enforcement flag lost")
	}
}

func TestRegistryIgnoresIncompleteEntries(t *testing.T) {
	r := NewRegistry(true)
	r.Register("orders", RegistryEntry{})
	r.Register("", RegistryEntry{Service: &Service{}})
	if len(r.Databases()) != 0 {
		t.Fatalf("incomplete entries registered: %v", r.Databases())
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry(true)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("db%d", i)
			r.Register(name, RegistryEntry{Service: &Service{}})
			r.Lookup(name)
			r.Databases()
		}(i)
	}
	wg.Wait()
	if len(r.Databases()) != 16 {
		t.Fatalf("databases = %d", len(r.Databases()))
	}
}
