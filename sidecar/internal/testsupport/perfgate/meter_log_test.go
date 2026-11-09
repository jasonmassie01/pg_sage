package perfgate

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

func TestMeterLogDedupesByAt(t *testing.T) {
	l := NewMeterLog()
	c := knownCost(0, 100, 15)
	if !l.AddCost(c) {
		t.Fatal("first reading not recorded")
	}
	if l.AddCost(c) {
		t.Fatal("the same reading (same At) recorded twice")
	}
	if l.AddCost(selfcost.Cost{}) {
		t.Fatal("a zero reading (no measurement yet) was recorded")
	}
	got := l.Costs(meterEpoch, meterEpoch.Add(time.Hour))
	if len(got) != 1 || got[0].DBTimeMsPerCycle != 100 {
		t.Fatalf("costs = %+v, want the one reading", got)
	}
}

func TestMeterLogCostsWithinPhase(t *testing.T) {
	l := NewMeterLog()
	// Windows of 15 s ending at +15 s, +30 s, +45 s, +60 s.
	for i := 0; i < 4; i++ {
		l.AddCost(knownCost(i, float64(i), 15))
	}
	from, to := meterEpoch.Add(15*time.Second), meterEpoch.Add(45*time.Second)
	got := l.Costs(from, to)
	// +15 s starts before the phase; +60 s ends after it.
	if len(got) != 2 || got[0].DBTimeMsPerCycle != 1 || got[1].DBTimeMsPerCycle != 2 {
		t.Fatalf("costs = %+v, want the readings whose window lies in the phase", got)
	}
	if got := l.Costs(to, from); len(got) != 0 {
		t.Fatalf("an inverted range gave %+v", got)
	}
	var empty *MeterLog
	if got := empty.Costs(from, to); got != nil {
		t.Fatalf("nil log gave %+v", got)
	}
}

func TestMeterLogPeriodIsTheMedianSnapshotGap(t *testing.T) {
	l := NewMeterLog()
	for _, s := range []int{0, 30, 60, 60, 95, 120} { // one late cycle, one duplicate
		l.AddSnapshot(meterEpoch.Add(time.Duration(s) * time.Second))
	}
	got := l.Period(meterEpoch, meterEpoch.Add(time.Hour))
	if got != 30*time.Second {
		t.Fatalf("period = %s, want the median gap 30s", got)
	}
	if got := l.Period(meterEpoch.Add(100*time.Second), meterEpoch.Add(time.Hour)); got != 0 {
		t.Fatalf("one snapshot in range gave period %s, want 0 (unknown)", got)
	}
	if got := NewMeterLog().Period(meterEpoch, meterEpoch.Add(time.Hour)); got != 0 {
		t.Fatalf("no snapshots gave period %s, want 0", got)
	}
	if l.AddSnapshot(time.Time{}) {
		t.Fatal("a zero snapshot time was recorded")
	}
}

// The runtime test samples from a ticker goroutine while the phase readers
// run: adds and reads must be safe together (-race is unavailable on this
// machine; the counts below catch lost or doubled writes).
func TestMeterLogConcurrentAccess(t *testing.T) {
	l := NewMeterLog()
	const writers, each = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c := knownCost(w*each+i, 10, 15)
				l.AddCost(c)
				l.AddCost(c) // every writer also repeats its reading
				l.AddSnapshot(c.At)
				_ = l.Costs(meterEpoch, meterEpoch.Add(24*time.Hour))
				_ = l.Period(meterEpoch, meterEpoch.Add(24*time.Hour))
			}
		}(w)
	}
	wg.Wait()
	all := l.Costs(meterEpoch.Add(-time.Hour), meterEpoch.Add(24*time.Hour))
	if len(all) != writers*each {
		t.Fatalf("recorded %d readings, want %d", len(all), writers*each)
	}
	for i := 1; i < len(all); i++ {
		if !all[i].At.After(all[i-1].At) {
			t.Fatalf("readings not in time order at %d", i)
		}
	}
	if got := l.Period(meterEpoch, meterEpoch.Add(24*time.Hour)); got != 15*time.Second {
		t.Fatalf("period = %s, want 15s", got)
	}
}
