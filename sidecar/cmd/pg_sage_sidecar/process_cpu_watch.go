package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// cpuSinkWriteTimeout bounds each write of the process CPU finding to one
// database, so a database that hangs (a held lock, a stalled network)
// cannot stall the one watch that serves every database: the write fails
// and the watch moves on to the next database.
var cpuSinkWriteTimeout = 30 * time.Second

// fleetCPUSink keeps the process CPU finding (footprint-07) in the
// monitored databases' sage.findings: the watch raises it on the
// lowest-named live database and resolves the copies elsewhere.
type fleetCPUSink struct {
	mgr *fleet.DatabaseManager
}

func newFleetCPUSink(mgr *fleet.DatabaseManager) *fleetCPUSink {
	return &fleetCPUSink{mgr: mgr}
}

// Databases are the registered databases that have a pool and are
// connected, sorted by name.
func (s *fleetCPUSink) Databases() []string {
	if s == nil || s.mgr == nil {
		return nil
	}
	var out []string
	for name, inst := range s.mgr.Instances() {
		if inst != nil && inst.Pool != nil && inst.SnapshotStatus().Connected {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *fleetCPUSink) instance(database string) (*fleet.DatabaseInstance, error) {
	if s != nil && s.mgr != nil {
		if inst := s.mgr.GetInstance(database); inst != nil && inst.Pool != nil {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("database %q is not registered with a pool", database)
}

// Raise opens or refreshes the sidecar's sage_self_budget finding on
// database.
func (s *fleetCPUSink) Raise(ctx context.Context, database string, b selfbudget.Breach,
	loops []selfbudget.LoopCost) error {
	inst, err := s.instance(database)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cpuSinkWriteTimeout)
	defer cancel()
	return analyzer.UpsertFindings(ctx, inst.Pool,
		[]analyzer.Finding{analyzer.SidecarCPUFinding(b, loops)})
}

// Clear resolves database's open sidecar sage_self_budget finding.
func (s *fleetCPUSink) Clear(ctx context.Context, database string) error {
	inst, err := s.instance(database)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cpuSinkWriteTimeout)
	defer cancel()
	_, err = analyzer.ResolveSidecarSelfBudget(ctx, inst.Pool)
	return err
}

// globalCollectorInterval is the documented unit of
// self_budget.cpu_ms_per_cycle: the global collector.interval_seconds.
func globalCollectorInterval() time.Duration {
	if cfg == nil {
		return 0
	}
	return time.Duration(cfg.Collector.IntervalSeconds) * time.Second
}

func globalCPUBudget() float64 {
	if cfg == nil {
		return 0
	}
	return float64(cfg.SelfBudget.CPUMsPerCycle)
}

// startProcessCPUWatch starts the one process CPU watch of this sidecar
// and registers it for /metrics and the footprint API.
func startProcessCPUWatch(ctx context.Context, mgr *fleet.DatabaseManager) {
	w := selfbudget.NewProcessCPUWatch(selfbudget.WatchOptions{
		Read: selfbudget.ProcessCPU, Budget: globalCPUBudget,
		Cycle: globalCollectorInterval, Sink: newFleetCPUSink(mgr),
		Loops: selfbudget.Process()})
	selfbudget.SetProcessWatch(w)
	go w.Run(ctx, cpuWatchReporter(func(err error) {
		logWarn("self_budget", "process CPU finding: %v", err)
	}))
}

// cpuWatchReporter logs a tick's failed writes when they differ from the
// previous tick's, so a refusing database does not log every cycle; a
// clean tick forgets them, so the same failure after a recovery logs again.
func cpuWatchReporter(log func(error)) func(error) {
	last := ""
	return func(err error) {
		if err == nil {
			last = ""
			return
		}
		if msg := err.Error(); msg != last {
			last = msg
			log(err)
		}
	}
}
