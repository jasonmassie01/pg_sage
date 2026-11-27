package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// footprint-07 on real PostgreSQL: two monitored databases, a process CPU
// budget of 1 ms per cycle and a hot process. The breach is one
// sage_self_budget finding on the sidecar object across both databases
// (CHECK-10), on the lowest-named live database, and it moves and
// resolves with the watch.

// scriptedCPU is the process CPU counter and clock the watch reads.
type scriptedCPU struct {
	mu  sync.Mutex
	cpu time.Duration
	at  time.Time
}

func (s *scriptedCPU) read() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cpu, true
}

func (s *scriptedCPU) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.at
}

func (s *scriptedCPU) burn(wall, cpu time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at, s.cpu = s.at.Add(wall), s.cpu+cpu
}

type fleetCPURig struct {
	ctx   context.Context
	mgr   *fleet.DatabaseManager
	pools map[string]*pgxpool.Pool
	cpu   *scriptedCPU
	watch *selfbudget.ProcessCPUWatch
}

func newFleetCPURig(t *testing.T) *fleetCPURig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	c := config.DefaultConfig()
	c.SelfBudget.CPUMsPerCycle = 1
	r := &fleetCPURig{ctx: ctx, mgr: fleet.NewManager(c), pools: map[string]*pgxpool.Pool{},
		cpu: &scriptedCPU{at: time.Now(), cpu: time.Minute}}
	for _, name := range []string{"db2", "db1"} {
		p := clusterPool(t, ctx, "fp07"+name)
		r.pools[name] = p
		r.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: p,
			Status: &fleet.InstanceStatus{Connected: true}})
	}
	r.watch = selfbudget.NewProcessCPUWatch(selfbudget.WatchOptions{
		Read: r.cpu.read, Now: r.cpu.now,
		Budget: func() float64 { return float64(c.SelfBudget.CPUMsPerCycle) },
		Cycle:  func() time.Duration { return time.Minute },
		Sink:   newFleetCPUSink(r.mgr), Loops: selfbudget.NewLoops()})
	return r
}

func (r *fleetCPURig) tick(t *testing.T, wall, cpu time.Duration) {
	t.Helper()
	r.cpu.burn(wall, cpu)
	if err := r.watch.Tick(r.ctx); err != nil {
		t.Fatalf("watch tick: %v", err)
	}
}

// cpuFindings counts each database's open CPU breach findings and the
// identities they carry.
func (r *fleetCPURig) cpuFindings(t *testing.T) (map[string]int, map[string]bool) {
	t.Helper()
	counts, idents := map[string]int{}, map[string]bool{}
	for name, p := range r.pools {
		rows, err := p.Query(r.ctx, `SELECT object_type, object_identifier FROM sage.findings
			WHERE category = 'sage_self_budget' AND status = 'open'
			  AND detail->'breaches' @> '[{"resource":"cpu"}]'`)
		if err != nil {
			t.Fatalf("%s: read findings: %v", name, err)
		}
		for rows.Next() {
			var typ, ident string
			if err := rows.Scan(&typ, &ident); err != nil {
				t.Fatal(err)
			}
			counts[name]++
			idents[typ+"/"+ident] = true
		}
		rows.Close()
	}
	return counts, idents
}

func TestFleetSelfBudget_OneCPUFindingAcrossDatabases(t *testing.T) {
	r := newFleetCPURig(t)
	r.tick(t, 0, 0)                        // first observation: unknown
	r.tick(t, 30*time.Second, time.Second) // 2000 ms per 60 s cycle against 1
	counts, idents := r.cpuFindings(t)
	if counts["db1"]+counts["db2"] != 1 || counts["db1"] != 1 { // CHECK-10
		t.Fatalf("CPU findings per database = %v, want exactly one, on db1", counts)
	}
	if len(idents) != 1 || !idents["sidecar/sidecar"] {
		t.Fatalf("identities = %v, want sidecar/sidecar", idents)
	}
	var detail string
	if err := r.pools["db1"].QueryRow(r.ctx, `SELECT detail::text FROM sage.findings
		WHERE category = 'sage_self_budget' AND object_identifier = 'sidecar'
		  AND status = 'open'`).Scan(&detail); err != nil {
		t.Fatalf("read detail: %v", err)
	}
	if !strings.Contains(detail, `"used": 2000`) || !strings.Contains(detail, `"limit": 1`) {
		t.Fatalf("detail = %s, want 2000 ms used of 1", detail)
	}
	r.tick(t, time.Minute, time.Hour) // still over: refreshed, not duplicated
	if counts, _ := r.cpuFindings(t); counts["db1"] != 1 || counts["db2"] != 0 {
		t.Fatalf("after a second breach: %v, want still one on db1", counts)
	}
}

// State transitions: db1 disconnects, db2 carries the finding; db1
// returns and db2's copy resolves; under budget, every copy resolves.
func TestFleetSelfBudget_FindingMovesAndResolves(t *testing.T) {
	r := newFleetCPURig(t)
	r.tick(t, 0, 0)
	r.tick(t, time.Minute, time.Hour)
	db1 := r.mgr.GetInstance("db1")
	db1.UpdateStatus(func(s *fleet.InstanceStatus) { s.Connected = false })
	r.tick(t, time.Minute, time.Hour)
	if counts, _ := r.cpuFindings(t); counts["db2"] != 1 {
		t.Fatalf("db1 down: %v, want db2 to carry the finding", counts)
	}
	db1.UpdateStatus(func(s *fleet.InstanceStatus) { s.Connected = true })
	r.tick(t, time.Minute, time.Hour)
	if counts, _ := r.cpuFindings(t); counts["db1"] != 1 || counts["db2"] != 0 {
		t.Fatalf("db1 back: %v, want one on db1 only", counts)
	}
	r.tick(t, time.Minute, 0)
	if counts, _ := r.cpuFindings(t); counts["db1"]+counts["db2"] != 0 {
		t.Fatalf("under budget: %v, want every CPU finding resolved", counts)
	}
}

// The sink lists only live databases with a pool, sorted.
func TestFleetCPUSink_Databases(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "zeta",
		Status: &fleet.InstanceStatus{Connected: true}}) // no pool
	if got := newFleetCPUSink(mgr).Databases(); len(got) != 0 {
		t.Fatalf("databases = %v, want none without a pool", got)
	}
	if got := newFleetCPUSink(nil).Databases(); len(got) != 0 {
		t.Fatalf("nil manager databases = %v", got)
	}
	if err := newFleetCPUSink(mgr).Raise(context.Background(), "missing",
		selfbudget.Breach{}, nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("raise on an unknown database = %v, want an error naming it", err)
	}
}
