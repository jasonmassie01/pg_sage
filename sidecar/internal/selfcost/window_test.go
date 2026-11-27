package selfcost

import (
	"math"
	"sync"
	"testing"
	"time"
)

// footprint-08 review: the API reads the cost, the costliest statements
// and the per-component cost of one window; Window returns them under one
// lock so a reader never mixes two windows.

func windowReading(t0 time.Time, i int) Reading {
	// Each window adds i*i ms (and i calls) to the collector entry, so
	// consecutive windows differ and a mixed answer cannot add up.
	total := float64(i * (i + 1) * (2*i + 1) / 6)
	return Reading{At: t0.Add(time.Duration(i) * time.Minute), StatementsKnown: true,
		Statements: map[StatementKey]StatementCounters{
			key(1): {TimeMs: total, Calls: int64(i * (i + 1) / 2),
				Text: "SELECT /* pg_sage collector */ 1"}}}
}

func TestMeter_WindowNilAndBeforeWindow(t *testing.T) {
	var nilMeter *Meter
	if c, top, comps := nilMeter.Window(); c.Known || top != nil || comps != nil {
		t.Errorf("nil meter Window = %+v %v %v, want zero", c, top, comps)
	}
	m := NewMeter()
	m.Observe(windowReading(time.Now(), 1), time.Minute)
	c, top, comps := m.Window()
	if c.Known || top != nil || comps != nil {
		t.Errorf("first reading Window = %+v %v %v, want no window", c, top, comps)
	}
}

func TestMeter_WindowIsOneWindow(t *testing.T) {
	m := NewMeter()
	t0 := time.Now()
	m.Observe(windowReading(t0, 1), time.Minute)
	m.Observe(windowReading(t0, 2), time.Minute)
	c, top, comps := m.Window()
	if !c.DBTimeKnown || len(top) != 1 || len(comps) != 1 {
		t.Fatalf("Window = %+v %v %v, want one known window", c, top, comps)
	}
	if top[0].TimeMs != 4 || comps[0].Component != "collector" ||
		math.Abs(comps[0].DBTimeMsPerHour-240) > 1e-9 {
		t.Errorf("Window top %+v comps %+v, want 4 ms, collector 240 ms/h", top, comps)
	}
	top[0].TimeMs, comps[0].DBTimeMsPerHour = -1, -1
	if _, top2, comps2 := m.Window(); top2[0].TimeMs != 4 || comps2[0].DBTimeMsPerHour != 240 {
		t.Errorf("Window returned the meter's own slices: %v %v", top2, comps2)
	}
}

// Concurrent access: while windows of different sizes are observed, each
// answer's per-component time is the same window's total and statements.
func TestMeter_WindowConsistentUnderObserve(t *testing.T) {
	m := NewMeter()
	t0 := time.Now()
	m.Observe(windowReading(t0, 1), time.Minute)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 2; i < 400; i++ {
			m.Observe(windowReading(t0, i), time.Minute)
		}
	}()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				c, top, comps := m.Window()
				if !c.DBTimeKnown {
					continue
				}
				perHour := c.DBTimeMsPerCycle * 3600 / c.CycleSeconds
				if len(comps) != 1 || len(top) != 1 ||
					math.Abs(comps[0].DBTimeMsPerHour-perHour) > 1e-6 ||
					math.Abs(top[0].TimeMs*3600/c.WindowSeconds-perHour) > 1e-6 {
					t.Errorf("mixed windows: cost %v ms/h, top %+v, comps %+v",
						perHour, top, comps)
					return
				}
			}
		}()
	}
	wg.Wait()
}
