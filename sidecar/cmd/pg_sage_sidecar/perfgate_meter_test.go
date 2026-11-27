//go:build perfgate

package main

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfconfig"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// compressSelfConfig runs the live derivation pass three times per steady
// window instead of hourly, so its evidence reads are measured; the
// shipped interval is restored after the test.
func compressSelfConfig(t *testing.T, timing perfgate.Timing) {
	t.Helper()
	old := selfConfigInterval
	selfConfigInterval = timing.Window / 3
	t.Cleanup(func() { selfConfigInterval = old })
}

// requireSteadyMeasured fails unless the steady phase measured what its
// gates judge: a live derivation pass, and enough sage.findings updates
// for gate F.
func requireSteadyMeasured(t *testing.T, steady perfgate.Phase) {
	t.Helper()
	requireLiveDerivation(t, steady)
	requireFindingsJudged(t, steady)
}

// requireFindingsJudged fails unless the phase holds the HotMinUpdates
// updates of sage.findings gate F needs to judge it: the refreshes of the
// open findings each analyzer cycle makes (perfConfig keeps pg_sage's
// self-budget finding open). Fewer, and a refresh that stopped being HOT
// would pass unjudged.
func requireFindingsJudged(t *testing.T, phase perfgate.Phase) {
	t.Helper()
	least := perfgate.DefaultBudgets().HotMinUpdates
	var updates int64
	for _, d := range phase.Tables {
		if d.Name == "sage.findings" {
			updates = d.Updates
		}
	}
	if updates < least {
		t.Errorf("%s phase: %d updates of sage.findings, fewer than the %d gate F judges; "+
			"its refreshes went unmeasured (perfConfig's one-block self budget keeps "+
			"pg_sage's own finding open so every cycle refreshes it)", phase.Name, updates, least)
	}
}

// requireLiveDerivation fails unless a live derivation pass ran in the
// phase: its catalog-size evidence read is among the phase's statements.
func requireLiveDerivation(t *testing.T, phase perfgate.Phase) {
	t.Helper()
	if !perfgate.HoldsStatement(phase.Statements, selfconfig.CatalogScanSQL) {
		t.Errorf("%s phase: no live derivation pass ran (selfconfig.CatalogScanSQL is not "+
			"in pg_stat_statements; the pass runs every %s)", phase.Name, selfConfigInterval)
	}
}

// startPerfMeter samples the standalone runtime's product meter (the
// analyzer's self-cost) and its collector's snapshot times every half
// interval until stopped; stopping takes a last sample.
func startPerfMeter(t *testing.T, timing perfgate.Timing) (*perfgate.MeterLog, func()) {
	t.Helper()
	name := resolveDBName()
	inst := fleetMgr.Instances()[name]
	if inst == nil || inst.Analyzer == nil || inst.Collector == nil {
		t.Fatalf("no standalone runtime %q with an analyzer and a collector to meter", name)
	}
	log := perfgate.NewMeterLog()
	sample := func() {
		log.AddCost(inst.Analyzer.SelfCost())
		if s := inst.Collector.LatestSnapshot(); s != nil {
			log.AddSnapshot(s.CollectedAt)
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(timing.Interval / 2)
		defer tick.Stop()
		for {
			sample()
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			sample()
		})
	}
	t.Cleanup(stop)
	return log, stop
}

// meterPerfPhase attaches the meter readings whose windows lie in
// [from, to] and the collector period observed there.
func meterPerfPhase(p *perfgate.Phase, log *perfgate.MeterLog, from, to time.Time) {
	p.Meter = log.Costs(from, to)
	p.CollectorPeriod = log.Period(from, to)
}

// perfSteadyWindow waits out the steady window that started at start,
// calling the API list endpoints halfway, and returns the endpoints'
// measurements, the sidecar CPU over the window with the API calls'
// CPU subtracted, and the window's end.
func perfSteadyWindow(t *testing.T, router http.Handler, session string, start time.Time,
	timing perfgate.Timing) ([]perfgate.Endpoint, time.Duration, time.Time) {
	t.Helper()
	cpuStart := perfProcessCPU(t)
	time.Sleep(timing.Window / 2)
	apiStart := perfProcessCPU(t)
	endpoints := callPerfEndpoints(router, session, perfEndpointSamples(t))
	apiCPU := perfProcessCPU(t) - apiStart
	time.Sleep(time.Until(start.Add(timing.Window)))
	cpu := perfProcessCPU(t) - cpuStart - apiCPU
	return endpoints, cpu, time.Now()
}
