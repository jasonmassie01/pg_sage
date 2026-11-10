package managedparam

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

// clouds-08: the databases of one provider instance are peers, so a
// parameter-group change one of them holds is not proposed again by the
// others.

func TestPeerDatabasesGroupsByResource(t *testing.T) {
	a, b, c := NewStore(nil), NewStore(nil), NewStore(nil)
	leaveA := JoinPeers("rds|peer-unit-1", "peer-unit-a", a)
	leaveB := JoinPeers("rds|peer-unit-1", "peer-unit-b", b)
	leaveC := JoinPeers("rds|peer-unit-2", "peer-unit-c", c)
	t.Cleanup(func() { leaveA(); leaveB(); leaveC() })
	if got := PeerDatabases("peer-unit-b"); !slices.Equal(got,
		[]string{"peer-unit-a", "peer-unit-b"}) {
		t.Fatalf("members of b = %v", got)
	}
	if got := PeerDatabases("peer-unit-c"); !slices.Equal(got, []string{"peer-unit-c"}) {
		t.Fatalf("another instance's members = %v", got)
	}
	if got := PeerDatabases("peer-unit-none"); got != nil {
		t.Fatalf("an unknown database has members %v", got)
	}
	others := otherPeers("rds|peer-unit-1", "peer-unit-a")
	if len(others) != 1 || others[0].database != "peer-unit-b" || others[0].store != b {
		t.Fatalf("peers of a = %+v, want only b", others)
	}
}

// State transition: leaving removes only that member; the last leave
// removes the group, and a stale leave keeps the member's replacement.
func TestPeerLeaveAndReplace(t *testing.T) {
	old, repl, b := NewStore(nil), NewStore(nil), NewStore(nil)
	leaveOld := JoinPeers("rds|peer-leave", "peer-leave-a", old)
	leaveRepl := JoinPeers("rds|peer-leave", "peer-leave-a", repl)
	leaveB := JoinPeers("rds|peer-leave", "peer-leave-b", b)
	leaveOld()
	others := otherPeers("rds|peer-leave", "peer-leave-b")
	if len(others) != 1 || others[0].store != repl {
		t.Fatalf("a stale leave removed the replacement: %+v", others)
	}
	leaveRepl()
	leaveRepl() // idempotent
	if got := PeerDatabases("peer-leave-b"); !slices.Equal(got, []string{"peer-leave-b"}) {
		t.Fatalf("after a leaves, members = %v", got)
	}
	leaveB()
	if got := PeerDatabases("peer-leave-b"); got != nil {
		t.Fatalf("the last leave kept members %v", got)
	}
	if peerGroupFor("rds|peer-leave") != nil {
		t.Fatal("the last leave kept the group")
	}
}

// Nil/empty: an empty resource or database, or a nil store, joins nothing
// and its leave is a no-op.
func TestPeerJoinIgnoresEmpty(t *testing.T) {
	for _, tc := range []struct {
		resource, database string
		store              *Store
	}{{"", "peer-empty-a", NewStore(nil)}, {"rds|peer-empty", "", NewStore(nil)},
		{"rds|peer-empty", "peer-empty-b", nil}} {
		leave := JoinPeers(tc.resource, tc.database, tc.store)
		if leave == nil {
			t.Fatalf("JoinPeers(%q, %q) returned a nil leave", tc.resource, tc.database)
		}
		leave()
	}
	if peerGroupFor("rds|peer-empty") != nil || PeerDatabases("peer-empty-a") != nil {
		t.Fatal("an empty join registered a peer")
	}
	if got := otherPeers("", "peer-empty-a"); got != nil {
		t.Fatalf("no resource has peers %v", got)
	}
}

// Concurrency: members joining and leaving one resource while peers are
// read (-race is unavailable; unlocked map access still fails).
func TestPeerConcurrentJoinLeave(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := fmt.Sprintf("peer-race-%d", i)
			for range 200 {
				JoinPeers("rds|peer-race", db, NewStore(nil))()
				_ = otherPeers("rds|peer-race", db)
				_ = PeerDatabases(db)
			}
		}()
	}
	wg.Wait()
	if peerGroupFor("rds|peer-race") != nil {
		t.Fatal("every member left but the group remains")
	}
}

// A reload builds a database's next generation (joining its new store)
// before the old one stops. When that reload is discarded, the new store
// leaves first: the running generation's store must stay the member, or
// its peers would propose the changes it holds a second time.
func TestPeerDiscardedReplacementKeepsTheRunningStore(t *testing.T) {
	running, candidate, b := NewStore(nil), NewStore(nil), NewStore(nil)
	leaveRunning := JoinPeers("rds|peer-discard", "peer-discard-a", running)
	leaveCandidate := JoinPeers("rds|peer-discard", "peer-discard-a", candidate)
	leaveB := JoinPeers("rds|peer-discard", "peer-discard-b", b)
	t.Cleanup(func() { leaveRunning(); leaveB() })
	if others := otherPeers("rds|peer-discard", "peer-discard-b"); len(others) != 1 ||
		others[0].store != candidate {
		t.Fatalf("the newest generation is not the member: %+v", others)
	}
	leaveCandidate()
	others := otherPeers("rds|peer-discard", "peer-discard-b")
	if len(others) != 1 || others[0].database != "peer-discard-a" ||
		others[0].store != running {
		t.Fatalf("after the discard, peers of b = %+v, want a's running store", others)
	}
	if got := PeerDatabases("peer-discard-b"); !slices.Equal(got,
		[]string{"peer-discard-a", "peer-discard-b"}) {
		t.Fatalf("after the discard, members = %v", got)
	}
}
