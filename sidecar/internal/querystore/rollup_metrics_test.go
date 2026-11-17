package querystore

import (
	"fmt"
	"sync"
	"testing"
)

// The rollup series of a database the process stops monitoring leave
// /metrics; the other databases' series stay.

func resetRollupMetrics(t *testing.T) {
	t.Helper()
	rollupMetrics.mu.Lock()
	hours, lag := rollupMetrics.hours, rollupMetrics.lag
	rollupMetrics.hours = map[rollupKey]uint64{}
	rollupMetrics.lag = map[string]float64{}
	rollupMetrics.mu.Unlock()
	t.Cleanup(func() {
		rollupMetrics.mu.Lock()
		rollupMetrics.hours, rollupMetrics.lag = hours, lag
		rollupMetrics.mu.Unlock()
	})
}

func TestForgetRollupDatabase_DropsOnlyThatDatabase(t *testing.T) {
	resetRollupMetrics(t)
	noteRollupHour("gone", "ok")
	noteRollupHour("gone", "failed")
	noteRollupHour("kept", "ok")
	setRollupLag("gone", 3)
	setRollupLag("kept", 1)
	ForgetRollupDatabase("gone")
	hours := RollupHourCounts()
	if len(hours) != 1 || hours[0] != (RollupHourCount{Database: "kept", Result: "ok",
		Count: 1}) {
		t.Fatalf("hour counts = %+v, want only kept/ok = 1", hours)
	}
	lags := RollupLags()
	if len(lags) != 1 || lags[0] != (RollupLag{Database: "kept", Hours: 1}) {
		t.Fatalf("lags = %+v, want only kept = 1", lags)
	}
}

func TestForgetRollupDatabase_UnknownAndEmptyNamesChangeNothing(t *testing.T) {
	resetRollupMetrics(t)
	ForgetRollupDatabase("never-seen") // nothing recorded yet: no panic
	setRollupLag("kept", 2)
	noteRollupHour("kept", "empty")
	ForgetRollupDatabase("")
	ForgetRollupDatabase("never-seen")
	if got := RollupLags(); len(got) != 1 || got[0].Hours != 2 {
		t.Fatalf("lags = %+v, want kept = 2", got)
	}
	if got := RollupHourCounts(); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("hour counts = %+v, want kept/empty = 1", got)
	}
}

func TestForgetRollupDatabase_ConcurrentWithWritersAndReaders(t *testing.T) {
	resetRollupMetrics(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		db := fmt.Sprintf("db%d", i%4)
		wg.Add(3)
		go func() { defer wg.Done(); noteRollupHour(db, "ok"); setRollupLag(db, 1) }()
		go func() { defer wg.Done(); ForgetRollupDatabase("db0") }()
		go func() { defer wg.Done(); _, _ = RollupHourCounts(), RollupLags() }()
	}
	wg.Wait()
	ForgetRollupDatabase("db0")
	for _, l := range RollupLags() {
		if l.Database == "db0" {
			t.Fatalf("db0 lag survived the last forget: %+v", l)
		}
	}
	if got := len(RollupLags()); got != 3 {
		t.Fatalf("lags for %d databases, want db1..db3", got)
	}
}
