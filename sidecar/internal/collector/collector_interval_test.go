package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// footprint-01: collector.interval_seconds is a live key (self-config
// promotes it, the API and file edits change it), so the collector must
// re-read it between cycles and move its ticker to the new period.

// fakeTicker records every Reset the interval sync makes.
type fakeTicker struct {
	mu     sync.Mutex
	resets []time.Duration
}

func (f *fakeTicker) Reset(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets = append(f.resets, d)
}

func (f *fakeTicker) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.resets...)
}

type intervalLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *intervalLog) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+": "+fmt.Sprintf(msg, args...))
}

func (l *intervalLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// intervalCollector is a collector whose ticker runs at 60 s, as Run
// leaves it after startup. No database: the sync reads only the config.
func intervalCollector(t *testing.T) (*Collector, *config.Config, *intervalLog) {
	t.Helper()
	cfg := testConfig()
	logs := &intervalLog{}
	c := New(nil, cfg, 170000, logs.log)
	c.period.Store(int64(60 * time.Second))
	return c, cfg, logs
}

func setInterval(cfg *config.Config, secs int) {
	config.LockForHotReload()
	defer config.UnlockForHotReload()
	cfg.Collector.IntervalSeconds = secs
}

// State transition: 60 -> 180 between cycles resets the period exactly once.
func TestSyncIntervalResetsOnceWhenTheIntervalChanges(t *testing.T) {
	c, cfg, logs := intervalCollector(t)
	ft := &fakeTicker{}
	setInterval(cfg, 180)
	c.syncInterval(ft)
	c.syncInterval(ft)
	c.syncInterval(ft)
	got := ft.got()
	if len(got) != 1 || got[0] != 180*time.Second {
		t.Fatalf("resets %v, want exactly [3m0s]", got)
	}
	if c.Interval() != 180*time.Second {
		t.Fatalf("Interval() = %s, want 3m0s", c.Interval())
	}
	if !strings.Contains(logs.text(), "INFO: collector: interval changed 60s -> 180s") {
		t.Fatalf("no interval change logged: %q", logs.text())
	}
	if n := strings.Count(logs.text(), "interval changed"); n != 1 {
		t.Fatalf("interval change logged %d times, want 1: %q", n, logs.text())
	}
}

// Boundary: an unchanged value makes no Reset and logs nothing.
func TestSyncIntervalUnchangedMakesNoReset(t *testing.T) {
	c, _, logs := intervalCollector(t)
	ft := &fakeTicker{}
	for i := 0; i < 5; i++ {
		c.syncInterval(ft)
	}
	if got := ft.got(); len(got) != 0 {
		t.Fatalf("unchanged interval reset the ticker: %v", got)
	}
	if c.Interval() != 60*time.Second || logs.text() != "" {
		t.Fatalf("Interval() = %s, logs %q", c.Interval(), logs.text())
	}
}

// Boundary: 60 -> 61 s is a change (one second is not rounded away), and
// changing back resets again.
func TestSyncIntervalFollowsEveryChange(t *testing.T) {
	c, cfg, _ := intervalCollector(t)
	ft := &fakeTicker{}
	setInterval(cfg, 61)
	c.syncInterval(ft)
	setInterval(cfg, 60)
	c.syncInterval(ft)
	got := ft.got()
	if len(got) != 2 || got[0] != 61*time.Second || got[1] != 60*time.Second {
		t.Fatalf("resets %v, want [1m1s 1m0s]", got)
	}
}

// Nil/zero: a zero or negative interval keeps the period in use (a zero
// Reset panics) and warns, naming the key.
func TestSyncIntervalNonPositiveKeepsThePeriod(t *testing.T) {
	for _, secs := range []int{0, -30} {
		t.Run(fmt.Sprint(secs), func(t *testing.T) {
			c, cfg, logs := intervalCollector(t)
			ft := &fakeTicker{}
			setInterval(cfg, secs)
			c.syncInterval(ft)
			if got := ft.got(); len(got) != 0 {
				t.Fatalf("non-positive interval reset the ticker: %v", got)
			}
			if c.Interval() != 60*time.Second {
				t.Fatalf("Interval() = %s, want the 60 s in use", c.Interval())
			}
			want := fmt.Sprintf("WARN: collector: collector.interval_seconds=%d is not "+
				"positive; keeping 60s", secs)
			if !strings.Contains(logs.text(), want) {
				t.Fatalf("logs %q, want %q", logs.text(), want)
			}
		})
	}
}

// Nil/zero: before Run the collector runs no ticker; Interval() is 0
// (unknown), never the configured value it is not running at.
func TestIntervalIsZeroBeforeRun(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	if c.Interval() != 0 {
		t.Fatalf("Interval() before Run = %s, want 0", c.Interval())
	}
}

// State transition: while dormant the ticker runs at the dormant interval;
// leaving dormancy applies the interval configured now, not the one in
// force when the breaker tripped.
func TestSyncIntervalAfterDormancyAppliesTheNewValue(t *testing.T) {
	c, cfg, logs := intervalCollector(t)
	ft := &fakeTicker{}
	c.usePeriod(ft, cfg.Safety.DormantInterval())
	setInterval(cfg, 180)
	c.syncInterval(ft)
	got := ft.got()
	want := []time.Duration{cfg.Safety.DormantInterval(), 180 * time.Second}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resets %v, want %v", got, want)
	}
	if c.Interval() != 180*time.Second {
		t.Fatalf("Interval() = %s, want 3m0s", c.Interval())
	}
	if !strings.Contains(logs.text(), "interval changed 600s -> 180s") {
		t.Fatalf("restore not logged: %q", logs.text())
	}
}

// Boundary: leaving dormancy with the dormant interval equal to the
// configured one needs no Reset.
func TestSyncIntervalAfterDormancyAtTheSamePeriod(t *testing.T) {
	c, cfg, _ := intervalCollector(t)
	ft := &fakeTicker{}
	c.usePeriod(ft, cfg.Safety.DormantInterval())
	setInterval(cfg, int(cfg.Safety.DormantInterval()/time.Second))
	c.syncInterval(ft)
	if got := ft.got(); len(got) != 1 {
		t.Fatalf("resets %v, want only the dormant one", got)
	}
}

// Concurrency: the config is written under the hot-reload lock (as
// selfconfig and the API do) while cycles sync; every Reset is a value
// that was written, and the last write wins.
func TestSyncIntervalUnderConcurrentHotReload(t *testing.T) {
	c, cfg, _ := intervalCollector(t)
	ft := &fakeTicker{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				setInterval(cfg, 60+(i%4)*30)
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		c.syncInterval(ft)
		_ = c.Interval()
	}
	close(stop)
	wg.Wait()
	setInterval(cfg, 300)
	c.syncInterval(ft)
	allowed := map[time.Duration]bool{60 * time.Second: true, 90 * time.Second: true,
		120 * time.Second: true, 150 * time.Second: true, 300 * time.Second: true}
	got := ft.got()
	for _, d := range got {
		if !allowed[d] {
			t.Fatalf("reset to %s, which was never configured", d)
		}
	}
	if got[len(got)-1] != 300*time.Second || c.Interval() != 300*time.Second {
		t.Fatalf("last reset %s, Interval() %s, want 5m0s", got[len(got)-1], c.Interval())
	}
}

// Integration (real Postgres): a cycle that leaves the dormant state
// moves the ticker to the interval configured now.
func TestCycleLeavingDormancyUsesTheConfiguredInterval(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()
	cfg.Safety.CPUCeilingPct = 100
	logs := &intervalLog{}
	c := New(pool, cfg, 170000, logs.log)
	c.period.Store(int64(60 * time.Second))
	ft := &fakeTicker{}
	c.breaker.isDormant = true
	c.usePeriod(ft, cfg.Safety.DormantInterval())
	setInterval(cfg, 120)
	c.cycle(context.Background(), ft)
	if c.LatestSnapshot() == nil {
		t.Fatalf("cycle collected nothing: %s", logs.text())
	}
	got := ft.got()
	if len(got) != 2 || got[1] != 120*time.Second {
		t.Fatalf("resets %v, want the dormant interval then 2m0s", got)
	}
	if c.Interval() != 120*time.Second {
		t.Fatalf("Interval() = %s, want 2m0s", c.Interval())
	}
}

// Integration (real Postgres): an ordinary cycle picks up a changed
// interval, so a promotion reaches the ticker within one cycle.
func TestCyclePicksUpAChangedInterval(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()
	cfg.Safety.CPUCeilingPct = 100
	logs := &intervalLog{}
	c := New(pool, cfg, 170000, logs.log)
	c.period.Store(int64(60 * time.Second))
	ft := &fakeTicker{}
	c.cycle(context.Background(), ft)
	if got := ft.got(); len(got) != 0 {
		t.Fatalf("unchanged interval reset the ticker: %v", got)
	}
	setInterval(cfg, 120)
	c.cycle(context.Background(), ft)
	if got := ft.got(); len(got) != 1 || got[0] != 120*time.Second {
		t.Fatalf("resets %v, want [2m0s]", got)
	}
	if !strings.Contains(logs.text(), "interval changed 60s -> 120s") {
		t.Fatalf("no change logged: %s", logs.text())
	}
}
