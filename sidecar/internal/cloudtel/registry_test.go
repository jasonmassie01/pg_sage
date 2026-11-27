package cloudtel

import (
	"fmt"
	"sync"
	"testing"
)

// fakeStatus is a non-Runtime status source, like the Supabase adapter.
type fakeStatus struct{ database, provider string }

func (f *fakeStatus) Database() string { return f.database }

func (f *fakeStatus) Status() Status {
	return Status{Database: f.database, Provider: f.provider, Withhold: []string{}}
}

func registeredStatus(database string) (Status, bool) {
	for _, st := range Statuses() {
		if st.Database == database {
			return st, true
		}
	}
	return Status{}, false
}

func TestRegistryServesNonRuntimeSources(t *testing.T) {
	src := &fakeStatus{database: "reg-supabase", provider: "supabase"}
	Register(src)
	t.Cleanup(func() { Unregister(src) })
	st, ok := registeredStatus("reg-supabase")
	if !ok || st.Provider != "supabase" {
		t.Fatalf("status = %#v %v, want a supabase entry", st, ok)
	}
	// Lookup feeds runtime-only consumers (backup posture, provider ref).
	if rt := Lookup("reg-supabase"); rt != nil {
		t.Fatalf("Lookup returned %#v for a status-only source", rt)
	}
	Unregister(src)
	if _, ok := registeredStatus("reg-supabase"); ok {
		t.Fatal("unregistered source still listed")
	}
}

func TestRegistryUnregisterKeepsReplacement(t *testing.T) {
	old := &fakeStatus{database: "reg-replace", provider: "supabase"}
	replacement := Unavailable("reg-replace", "rds", "no credentials")
	Register(old)
	Register(replacement)
	t.Cleanup(func() { Unregister(replacement) })
	Unregister(old)
	st, ok := registeredStatus("reg-replace")
	if !ok || st.Provider != "rds" || Lookup("reg-replace") != replacement {
		t.Fatalf("stale unregister removed its replacement: %#v %v", st, ok)
	}
}

func TestRegistryIgnoresNilSources(t *testing.T) {
	before := len(Statuses())
	Register(nil)
	Register((*Runtime)(nil))
	Register((*fakeStatus)(nil))
	Unregister(nil)
	Unregister((*Runtime)(nil))
	Unregister((*fakeStatus)(nil))
	if got := len(Statuses()); got != before {
		t.Fatalf("nil registrations changed the registry: %d -> %d", before, got)
	}
}

// -race is unavailable on this machine; this still fails on a map
// access outside the lock ("concurrent map read and map write").
func TestRegistryConcurrentRegisterAndStatuses(t *testing.T) {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := &fakeStatus{database: fmt.Sprintf("reg-race-%d", i), provider: "supabase"}
			for {
				select {
				case <-stop:
					Unregister(src)
					return
				default:
					Register(src)
					Unregister(src)
				}
			}
		}()
	}
	for range 2000 {
		for _, st := range Statuses() {
			if st.Database == "" || st.Provider == "" {
				t.Fatalf("torn status %#v", st)
			}
		}
	}
	close(stop)
	wg.Wait()
	for i := range 4 {
		if _, ok := registeredStatus(fmt.Sprintf("reg-race-%d", i)); ok {
			t.Fatalf("reg-race-%d left registered", i)
		}
	}
}
