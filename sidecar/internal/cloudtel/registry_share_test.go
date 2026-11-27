package cloudtel

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// clouds-08: databases on one provider instance share one runtime, so
// the registry serves the instance once, naming every member database,
// while Lookup still answers for each member.

func sharedStatus(t *testing.T, member string) (Status, int) {
	t.Helper()
	var found Status
	n := 0
	for _, st := range Statuses() {
		if slices.Contains(st.Databases, member) {
			found = st
			n++
		}
	}
	return found, n
}

func TestRegistryShareServesTheInstanceOnce(t *testing.T) {
	rt := Unavailable("share-a", "rds", "no AWS credentials")
	for _, db := range []string{"share-c", "share-a", "share-b"} {
		RegisterMember(db, rt)
	}
	t.Cleanup(func() {
		for _, db := range []string{"share-a", "share-b", "share-c"} {
			UnregisterMember(db, rt)
		}
	})
	st, n := sharedStatus(t, "share-a")
	if n != 1 {
		t.Fatalf("instance listed %d times, want once", n)
	}
	if !slices.Equal(st.Databases, []string{"share-a", "share-b", "share-c"}) ||
		st.Database != "share-a" || st.Provider != "rds" {
		t.Fatalf("status = database %q members %v provider %q", st.Database,
			st.Databases, st.Provider)
	}
	for _, db := range []string{"share-a", "share-b", "share-c"} {
		if Lookup(db) != rt {
			t.Fatalf("Lookup(%s) is not the shared runtime", db)
		}
	}
}

// State transition: a member leaving keeps the others; the status names
// the remaining members and is labelled by the first of them.
func TestRegistryShareMemberLeaves(t *testing.T) {
	rt := Unavailable("share-leave-a", "rds", "no AWS credentials")
	RegisterMember("share-leave-a", rt)
	RegisterMember("share-leave-b", rt)
	t.Cleanup(func() { UnregisterMember("share-leave-b", rt) })
	UnregisterMember("share-leave-a", rt)
	if Lookup("share-leave-a") != nil {
		t.Fatal("a departed member is still looked up")
	}
	st, n := sharedStatus(t, "share-leave-b")
	if n != 1 || st.Database != "share-leave-b" ||
		!slices.Equal(st.Databases, []string{"share-leave-b"}) {
		t.Fatalf("after a leaves: n=%d database %q members %v", n, st.Database, st.Databases)
	}
	UnregisterMember("share-leave-b", rt)
	if _, n := sharedStatus(t, "share-leave-b"); n != 0 {
		t.Fatal("the last member left but the instance is still listed")
	}
}

// Boundary: two instances are two entries, each with its own member.
func TestRegistryShareKeepsDistinctInstancesApart(t *testing.T) {
	one := Unavailable("share-one", "rds", "no AWS credentials")
	two := Unavailable("share-two", "rds", "no AWS credentials")
	RegisterMember("share-one", one)
	RegisterMember("share-two", two)
	t.Cleanup(func() {
		UnregisterMember("share-one", one)
		UnregisterMember("share-two", two)
	})
	a, na := sharedStatus(t, "share-one")
	b, nb := sharedStatus(t, "share-two")
	if na != 1 || nb != 1 || !slices.Equal(a.Databases, []string{"share-one"}) ||
		!slices.Equal(b.Databases, []string{"share-two"}) {
		t.Fatalf("distinct instances merged: %v / %v", a.Databases, b.Databases)
	}
}

// Register (one database per source) still lists the source's own
// database as its only member.
func TestRegistryShareRegisterListsItsDatabase(t *testing.T) {
	src := &fakeStatus{database: "share-legacy", provider: "supabase"}
	Register(src)
	t.Cleanup(func() { Unregister(src) })
	st, n := sharedStatus(t, "share-legacy")
	if n != 1 || st.Database != "share-legacy" ||
		!slices.Equal(st.Databases, []string{"share-legacy"}) {
		t.Fatalf("legacy registration = n=%d %q %v", n, st.Database, st.Databases)
	}
}

// Invalid input: an empty member name or a nil source registers nothing,
// and a stale unregister keeps the member's replacement.
func TestRegistryShareRejectsEmptyAndStale(t *testing.T) {
	before := len(Statuses())
	RegisterMember("", Unavailable("x", "rds", "r"))
	RegisterMember("share-nil", nil)
	RegisterMember("share-nil", (*Runtime)(nil))
	if got := len(Statuses()); got != before {
		t.Fatalf("invalid registrations changed the registry: %d -> %d", before, got)
	}
	old := Unavailable("share-stale", "rds", "old")
	repl := Unavailable("share-stale", "rds", "replacement")
	RegisterMember("share-stale", old)
	RegisterMember("share-stale", repl)
	t.Cleanup(func() { UnregisterMember("share-stale", repl) })
	UnregisterMember("share-stale", old)
	if Lookup("share-stale") != repl {
		t.Fatal("a stale unregister removed the member's replacement")
	}
}

// Concurrency: members joining and leaving one runtime while statuses are
// read never tear an entry (-race is unavailable; a map access outside the
// lock still fails with "concurrent map read and map write").
func TestRegistryShareConcurrentMembers(t *testing.T) {
	rt := Unavailable("share-race-0", "rds", "no AWS credentials")
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := fmt.Sprintf("share-race-%d", i)
			for range 200 {
				RegisterMember(db, rt)
				UnregisterMember(db, rt)
			}
			RegisterMember(db, rt)
		}()
	}
	for range 200 {
		if _, n := sharedStatus(t, "share-race-0"); n > 1 {
			t.Fatalf("shared runtime listed %d times", n)
		}
	}
	wg.Wait()
	st, n := sharedStatus(t, "share-race-0")
	if n != 1 || len(st.Databases) != 10 {
		t.Fatalf("after the race: n=%d members %v", n, st.Databases)
	}
	for i := range 10 {
		UnregisterMember(fmt.Sprintf("share-race-%d", i), rt)
	}
	if _, n := sharedStatus(t, "share-race-0"); n != 0 {
		t.Fatal("members left registered")
	}
}
