package fleetlearn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// countingServer accepts TCP connections, counts them and hangs up. A pool
// pointed at it never runs a query, but every attempt to run one dials, so
// a zero count proves the code under test issued no query at all.
type countingServer struct {
	addr string
	n    atomic.Int64
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c := &countingServer{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.n.Add(1)
			_ = conn.Close()
		}
	}()
	return c
}

// pool is a lazy pool on the counting server (pgxpool.New does not dial).
func (c *countingServer) pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(),
		"postgres://x@"+c.addr+"/x?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// logRecorder collects the service's log lines.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(level, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+" "+fmt.Sprintf(format, args...))
}

func (r *logRecorder) count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

const skipLogLine = "fleet learning: skipped, no peer in boundary"

type unitFleet struct {
	control *countingServer
	db      map[string]*countingServer
	srcs    []DatabaseSource
	mu      sync.Mutex
	logs    *logRecorder
	svc     *Service
}

// newUnitFleet builds a service whose control and source pools all point
// at counting servers; set swaps the sources the service sees.
func newUnitFleet(t *testing.T) *unitFleet {
	t.Helper()
	u := &unitFleet{control: newCountingServer(t), db: map[string]*countingServer{},
		logs: &logRecorder{}}
	u.svc = NewService(NewStore(u.control.pool(t), "unit-scope"), func() []DatabaseSource {
		u.mu.Lock()
		defer u.mu.Unlock()
		return append([]DatabaseSource(nil), u.srcs...)
	}, Settings{}, u.logs.log)
	return u
}

// source returns a source named name in boundary on its own counting server.
func (u *unitFleet) source(t *testing.T, name, boundary string) DatabaseSource {
	t.Helper()
	srv := newCountingServer(t)
	u.db[name] = srv
	return DatabaseSource{Name: name, Boundary: boundary, Pool: srv.pool(t)}
}

func (u *unitFleet) set(srcs ...DatabaseSource) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.srcs = srcs
}

func (u *unitFleet) dials() int64 {
	n := u.control.n.Load()
	for _, s := range u.db {
		n += s.n.Load()
	}
	return n
}

func assertSkipped(t *testing.T, u *unitFleet, label string) {
	t.Helper()
	res, err := u.svc.RunCycle(context.Background(), Fence{})
	if err != nil {
		t.Fatalf("%s: skipped cycle err = %v", label, err)
	}
	if res.Skipped != SkipNoPeer || res.Databases != 0 || len(res.Failed) != 0 ||
		res.Failed == nil {
		t.Fatalf("%s: result = %+v, want skipped %q, 0 databases, no failures",
			label, res, SkipNoPeer)
	}
	if n := u.dials(); n != 0 {
		t.Fatalf("%s: a skipped cycle dialled the databases %d times, want 0", label, n)
	}
}

func TestRunCycle_OneSourceIssuesNoQueryAndNoWrite(t *testing.T) {
	u := newUnitFleet(t)
	u.set(u.source(t, "only", ""))
	assertSkipped(t, u, "standalone")
}

func TestRunCycle_TwoSourcesInDifferentTenantsSkip(t *testing.T) {
	u := newUnitFleet(t)
	u.set(u.source(t, "a", "tenant:one"), u.source(t, "b", "tenant:two"))
	assertSkipped(t, u, "different tenants")
	u2 := newUnitFleet(t)
	u2.set(u2.source(t, "a", ""), u2.source(t, "b", "tenant:two"))
	assertSkipped(t, u2, "operator and tenant")
}

func TestRunCycle_NoSourcesAndNoPoolsSkipWithoutPanic(t *testing.T) {
	u := newUnitFleet(t)
	assertSkipped(t, u, "no sources")
	u.set(DatabaseSource{Name: "a"}, DatabaseSource{Name: "b"})
	assertSkipped(t, u, "sources without pools")
	u.set(u.source(t, "a", ""), DatabaseSource{Name: "b"})
	assertSkipped(t, u, "one pool, one without")
	// A nil sources func defaults to none.
	svc := NewService(NewStore(newCountingServer(t).pool(t), "s"), nil, Settings{}, nil)
	res, err := svc.RunCycle(context.Background(), Fence{})
	if err != nil || res.Skipped != SkipNoPeer {
		t.Fatalf("nil sources = %+v, %v; want skipped", res, err)
	}
}

func TestRunCycle_SameBoundaryPairIsRead(t *testing.T) {
	u := newUnitFleet(t)
	u.set(u.source(t, "a", "tenant:one"), u.source(t, "b", "tenant:one"),
		u.source(t, "lone", "tenant:two"))
	res, err := u.svc.RunCycle(context.Background(), Fence{})
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if res.Skipped != "" {
		t.Fatalf("an eligible fleet was skipped: %+v", res)
	}
	// Every source is read once the fleet is eligible, the lone tenant too
	// (its fingerprint waits for a peer; LookAlikes keeps it to itself).
	for _, name := range []string{"a", "b", "lone"} {
		if u.db[name].n.Load() == 0 {
			t.Fatalf("source %s was not read in an eligible cycle", name)
		}
	}
	if strings.Join(res.Failed, ",") != "a,b,lone" || res.Databases != 0 {
		t.Fatalf("result = %+v, want all three attempted and failed", res)
	}
	if u.logs.count(skipLogLine) != 0 {
		t.Fatal("an eligible cycle logged the skip")
	}
}

func TestRunCycle_SkipLogsOncePerTransition(t *testing.T) {
	u := newUnitFleet(t)
	one := u.source(t, "a", "")
	u.set(one)
	for i := 0; i < 3; i++ {
		assertSkipped(t, u, fmt.Sprintf("interval %d", i))
	}
	if n := u.logs.count(skipLogLine); n != 1 {
		t.Fatalf("3 skipped intervals logged the skip %d times, want 1", n)
	}
	if n := u.logs.count("INFO " + skipLogLine); n != 1 {
		t.Fatalf("the skip must be logged at INFO: %v", u.logs.lines)
	}
	u.set(one, u.source(t, "b", ""))
	if res, _ := u.svc.RunCycle(context.Background(), Fence{}); res.Skipped != "" {
		t.Fatalf("two peers still skipped: %+v", res)
	}
	u.set(one)
	u.db = map[string]*countingServer{"a": u.db["a"]}
	before := u.db["a"].n.Load()
	for i := 0; i < 2; i++ {
		res, _ := u.svc.RunCycle(context.Background(), Fence{})
		if res.Skipped != SkipNoPeer {
			t.Fatalf("back to one source not skipped: %+v", res)
		}
	}
	if u.db["a"].n.Load() != before {
		t.Fatal("a skipped cycle after shrinking read the remaining database")
	}
	if n := u.logs.count(skipLogLine); n != 2 {
		t.Fatalf("re-entering the skip logged %d skips in total, want 2", n)
	}
}

func TestRunCycle_ConcurrentSkipsLogOnce(t *testing.T) {
	u := newUnitFleet(t)
	u.set(u.source(t, "a", ""))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, err := u.svc.RunCycle(context.Background(), Fence{}); err != nil ||
				res.Skipped != SkipNoPeer {
				t.Errorf("concurrent skip = %+v, %v", res, err)
			}
		}()
	}
	wg.Wait()
	if n := u.logs.count(skipLogLine); n != 1 {
		t.Fatalf("16 concurrent skipped cycles logged %d times, want 1", n)
	}
}

func TestRunCycle_FencedSkipWritesNothingAndSucceeds(t *testing.T) {
	u := newUnitFleet(t)
	u.set(u.source(t, "a", ""))
	res, err := u.svc.RunCycle(context.Background(), Fence{Holder: "me", Epoch: 3})
	if err != nil || res.Skipped != SkipNoPeer {
		t.Fatalf("fenced skip = %+v, %v; want a quiet skip", res, err)
	}
	if u.control.n.Load() != 0 {
		t.Fatal("a skipped cycle touched the control database")
	}
}

func TestEligible_Boundaries(t *testing.T) {
	p := &pgxpool.Pool{}
	cases := []struct {
		name string
		srcs []DatabaseSource
		want bool
	}{
		{"nil", nil, false},
		{"empty", []DatabaseSource{}, false},
		{"one", []DatabaseSource{{Name: "a", Pool: p}}, false},
		{"two operator", []DatabaseSource{{Name: "a", Pool: p}, {Name: "b", Pool: p}}, true},
		{"two tenants", []DatabaseSource{{Name: "a", Boundary: "tenant:x", Pool: p},
			{Name: "b", Boundary: "tenant:y", Pool: p}}, false},
		{"pair among three", []DatabaseSource{{Name: "a", Pool: p},
			{Name: "b", Boundary: "tenant:x", Pool: p},
			{Name: "c", Boundary: "tenant:x", Pool: p}}, true},
		{"second without pool", []DatabaseSource{{Name: "a", Pool: p}, {Name: "b"}}, false},
		{"same name twice", []DatabaseSource{{Name: "a", Pool: p}, {Name: "a", Pool: p}},
			false},
		{"unnamed", []DatabaseSource{{Pool: p}, {Name: "b", Pool: p}}, false},
	}
	for _, c := range cases {
		if got := eligible(c.srcs); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
	u := newUnitFleet(t)
	if u.svc.Eligible() {
		t.Fatal("a service with no sources is eligible")
	}
	u.set(u.source(t, "a", ""), u.source(t, "b", ""))
	if !u.svc.Eligible() || u.dials() != 0 {
		t.Fatalf("Eligible() = %v after %d dials; want true with no query",
			u.svc.Eligible(), u.dials())
	}
}
