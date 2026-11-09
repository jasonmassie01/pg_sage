package logwatch

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-08: cluster events (lines without a database) go to one owner, the
// lowest subscriber id, so a fleet raises one incident per cluster event.

func panicSignal() *rca.Signal {
	return &rca.Signal{ID: "log_panic_server_crash", FiredAt: time.Now(),
		Severity: "critical", Metrics: map[string]any{"error_level": "PANIC"}}
}

func dbSignal(db string) *rca.Signal {
	return &rca.Signal{ID: "log_deadlock", FiredAt: time.Now(),
		Severity: "warning", Metrics: map[string]any{"database": db}}
}

func countID(sigs []*rca.Signal, id string) int {
	n := 0
	for _, s := range sigs {
		if s.ID == id {
			n++
		}
	}
	return n
}

// Happy path (fleet): three subscribers and one PANIC give one copy, on
// the owner, tagged with the cluster scope.
func TestFanoutClusterEventGoesOnlyToOwner(t *testing.T) {
	f := NewLogFanout(&FileWatcher{}).WithClusterKey("pg1:5432")
	c := f.Subscribe("c")
	a := f.Subscribe("a")
	b := f.Subscribe("b")
	orig := panicSignal()
	f.distribute([]*rca.Signal{orig})

	got := a.Drain()
	if len(got) != 1 || got[0].ID != "log_panic_server_crash" {
		t.Fatalf("owner a got %+v, want the one PANIC", got)
	}
	m := got[0].Metrics
	if m["scope"] != "cluster" || m["cluster_key"] != "pg1:5432" ||
		m["error_level"] != "PANIC" {
		t.Fatalf("owner metrics = %v, want scope/cluster_key tags and the line's", m)
	}
	if dbs, _ := m["databases"].([]string); !slices.Equal(dbs, []string{"a", "b", "c"}) {
		t.Fatalf("databases = %v, want [a b c]", m["databases"])
	}
	if _, tagged := orig.Metrics["scope"]; tagged {
		t.Fatal("the watcher's signal was mutated; the owner must get a copy")
	}
	if n := len(b.Drain()) + len(c.Drain()); n != 0 {
		t.Fatalf("non-owners got %d cluster signals, want 0", n)
	}
}

// Boundary: a line that names a database reaches every subscriber, the
// non-owner whose database it is included, and is not tagged as cluster
// scope; the engines keep only their own database's lines.
func TestFanoutDatabaseLineReachesNonOwners(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	a := f.Subscribe("a")
	b := f.Subscribe("b")
	f.distribute([]*rca.Signal{dbSignal("b_db"), panicSignal(), dbSignal("a_db")})

	gotB := b.Drain()
	if len(gotB) != 2 || gotB[0].Metrics["database"] != "b_db" ||
		gotB[1].Metrics["database"] != "a_db" {
		t.Fatalf("non-owner b got %+v, want both database lines in order", gotB)
	}
	if _, tagged := gotB[0].Metrics["scope"]; tagged {
		t.Fatalf("database line tagged %v; only cluster events carry a scope",
			gotB[0].Metrics)
	}
	var ids []string
	for _, s := range a.Drain() {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids,
		[]string{"log_deadlock", "log_panic_server_crash", "log_deadlock"}) {
		t.Fatalf("owner a got %v, want arrival order deadlock, panic, deadlock", ids)
	}
}

// Single-database mode is unchanged: the one subscriber owns the cluster
// and gets every signal.
func TestFanoutSingleSubscriberGetsEverything(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	only := f.Subscribe("only")
	f.distribute([]*rca.Signal{panicSignal(), dbSignal("app")})
	got := only.Drain()
	if len(got) != 2 || got[0].ID != "log_panic_server_crash" ||
		got[1].ID != "log_deadlock" {
		t.Fatalf("single subscriber got %+v, want both signals", got)
	}
	if _, keyed := got[0].Metrics["cluster_key"]; keyed {
		t.Fatal("cluster_key tagged without a known key")
	}
}

// State transition: stopping the owner deletes its buffer (CHECK-25),
// moves ownership to the next id and hands it the undrained cluster
// events; its database lines are not handed over.
func TestFanoutStopOwnerMovesOwnership(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	a := f.Subscribe("a")
	b := f.Subscribe("b")
	c := f.Subscribe("c")
	f.distribute([]*rca.Signal{panicSignal(), dbSignal("a_db")})
	_ = b.Drain()
	before := bufferCount(f)
	a.Stop()
	if after := bufferCount(f); after != before-1 {
		t.Fatalf("buffers after Stop = %d, want %d", after, before-1)
	}
	got := b.Drain()
	if len(got) != 1 || got[0].ID != "log_panic_server_crash" {
		t.Fatalf("new owner b got %+v, want the handed-over PANIC only", got)
	}
	_ = c.Drain()
	f.distribute([]*rca.Signal{panicSignal()})
	if n := countID(b.Drain(), "log_panic_server_crash"); n != 1 {
		t.Fatalf("new owner b got %d new PANICs, want 1", n)
	}
	if n := len(c.Drain()); n != 0 {
		t.Fatalf("c got %d signals after ownership moved to b, want 0", n)
	}
	if got := a.Drain(); got != nil {
		t.Fatalf("stopped subscriber drained %+v, want nil", got)
	}
	if bufferCount(f) != 2 {
		t.Fatal("draining a stopped subscriber recreated its buffer")
	}
}

// State transition: stopping a non-owner deletes only its buffer and
// keeps ownership; Stop is idempotent.
func TestFanoutStopNonOwnerKeepsOwner(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	a := f.Subscribe("a")
	b := f.Subscribe("b")
	b.Stop()
	b.Stop()
	if n := bufferCount(f); n != 1 {
		t.Fatalf("buffers = %d, want 1", n)
	}
	f.distribute([]*rca.Signal{panicSignal()})
	if n := len(a.Drain()); n != 1 {
		t.Fatalf("owner a got %d, want 1", n)
	}
	if got := b.DrainCritical(); got != nil {
		t.Fatalf("stopped subscriber DrainCritical = %+v, want nil", got)
	}
}

// State transition: re-subscribing an id keeps its backlog for the new
// handle, and the replaced handle's Stop no longer removes it.
func TestFanoutResubscribeKeepsBacklog(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	old := f.Subscribe("a")
	f.distribute([]*rca.Signal{panicSignal(), dbSignal("a_db")})
	renewed := f.Subscribe("a")
	if got := old.Drain(); got != nil {
		t.Fatalf("replaced handle drained %+v, want nil", got)
	}
	old.Stop()
	if bufferCount(f) != 1 {
		t.Fatal("the replaced handle's Stop removed the live subscription")
	}
	if got := renewed.Drain(); len(got) != 2 {
		t.Fatalf("re-subscribed handle got %d signals, want the 2 of the backlog",
			len(got))
	}
}

// Nil/empty: zero subscribers drop the signals without a panic; nil and
// empty batches change nothing.
func TestFanoutZeroSubscribersDropsSignals(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	f.distribute([]*rca.Signal{panicSignal(), nil})
	if bufferCount(f) != 0 {
		t.Fatal("a signal with no subscriber created a buffer")
	}
	a := f.Subscribe("a")
	f.distribute(nil)
	f.distribute([]*rca.Signal{nil})
	if got := a.Drain(); len(got) != 0 {
		t.Fatalf("nil signals delivered: %+v", got)
	}
	a.Stop()
	a.Stop()
	if bufferCount(f) != 0 {
		t.Fatal("last subscriber's Stop left a buffer")
	}
}

// Concurrency: subscribers come and go while signals are distributed and
// drained. "a" never leaves, so it owns the cluster throughout and must
// receive every cluster event exactly once; no other id may see one.
func TestFanoutConcurrentSubscribeStopKeepsOneOwner(t *testing.T) {
	f := NewLogFanout(&FileWatcher{})
	owner := f.Subscribe("a")
	const batches = 200
	var wg sync.WaitGroup
	var leaked sync.Map
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				sub := f.Subscribe(fmt.Sprintf("z%d", w))
				if n := countID(sub.Drain(), "log_panic_server_crash"); n > 0 {
					leaked.Store(w, n)
				}
				sub.Stop()
			}
		}(w)
	}
	got := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < batches; i++ {
			f.distribute([]*rca.Signal{panicSignal(), dbSignal("x")})
			got += countID(owner.Drain(), "log_panic_server_crash")
		}
	}()
	wg.Wait()
	got += countID(owner.Drain(), "log_panic_server_crash")
	if got != batches {
		t.Fatalf("owner received %d cluster events, want %d", got, batches)
	}
	leaked.Range(func(k, v any) bool {
		t.Errorf("subscriber z%v received %v cluster events", k, v)
		return true
	})
	if n := bufferCount(f); n != 1 {
		t.Fatalf("buffers after all z* stopped = %d, want 1", n)
	}
}

func bufferCount(f *LogFanout) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.live) != len(f.buffers) {
		panic(fmt.Sprintf("live %d != buffers %d", len(f.live), len(f.buffers)))
	}
	return len(f.buffers)
}
