package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-04: each database subscribes to the cluster's log entries and stores
// the executed auto_explain plans it finds; rejections and store failures
// are counted per reason and summarised with the setting to change.

func loopPlanEntry(db string, queryID int64) logwatch.LogEntry {
	return logwatch.LogEntry{Database: db, Timestamp: time.Unix(1700000000, 0),
		Application: "app", Message: fmt.Sprintf("duration: 4.000 ms  plan:\n"+
			`{"Query Text":"SELECT 1","Query Identifier":%d,"Plan":{"Total Cost":1}}`,
			queryID)}
}

type planLogCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *planLogCapture) logFn(level, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, level+" "+fmt.Sprintf(format, args...))
}

func (c *planLogCapture) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func newTestPlanLoop(sub entryDrainer, store func(context.Context,
	autoexplain.ObservedPlan) error, logs *planLogCapture) *observedPlanLoop {
	return newObservedPlanLoop(sub, autoexplain.ObservedPlanSink{Database: "appdb",
		Store: store, MaxPerCycle: 100, LogFn: logs.logFn}, "appdb", logs.logFn)
}

func TestObservedPlanLoop_StoresAndCountsEachCycle(t *testing.T) {
	var stored []int64
	store := func(_ context.Context, p autoexplain.ObservedPlan) error {
		if p.QueryID == 13 {
			return errors.New("insert explain_cache: connection reset")
		}
		stored = append(stored, p.QueryID)
		return nil
	}
	noID := loopPlanEntry("appdb", 0)
	noID.Message = "duration: 1 ms  plan:\n" + `{"Query Text":"SELECT 1","Plan":{"Total Cost":1}}`
	sub := &fakeDrainer{batches: [][]logwatch.LogEntry{
		{loopPlanEntry("appdb", 11), noID, loopPlanEntry("appdb", 13)},
		{loopPlanEntry("appdb", 12)},
	}}
	logs := &planLogCapture{}
	l := newTestPlanLoop(sub, store, logs)
	l.tick(context.Background())
	l.tick(context.Background())
	l.tick(context.Background()) // nothing queued: no-op
	if len(stored) != 2 || stored[0] != 11 || stored[1] != 12 || l.stored != 2 {
		t.Fatalf("stored %v (count %d), want 11 then 12", stored, l.stored)
	}
	if l.rejected[autoexplain.RejectMissingQueryID] != 1 ||
		l.rejected[autoexplain.RejectStoreFailed] != 1 {
		t.Fatalf("rejected = %v", l.rejected)
	}
	out := logs.joined()
	if !strings.Contains(out, "queryid=13") || !strings.Contains(out, "log_verbose") {
		t.Fatalf("logs %q must name the failed queryid and the setting to change", out)
	}
}

func TestObservedPlanLoop_CountsEvictedEntries(t *testing.T) {
	sub := &evictingDrainer{evicted: 7}
	logs := &planLogCapture{}
	l := newTestPlanLoop(sub, func(context.Context, autoexplain.ObservedPlan) error {
		return nil
	}, logs)
	l.tick(context.Background())
	if l.evicted != 7 || !strings.Contains(logs.joined(), "7") {
		t.Fatalf("evicted = %d, logs %q", l.evicted, logs.joined())
	}
}

type evictingDrainer struct{ evicted int }

func (d *evictingDrainer) Drain() []logwatch.LogEntry { return nil }
func (d *evictingDrainer) TakeEvicted() int {
	n := d.evicted
	d.evicted = 0
	return n
}
func (d *evictingDrainer) Stop() {}

// State transition: the loop runs until its context ends, then stops the
// subscription.
func TestRunObservedPlans_StopsTheSubscriptionOnCancel(t *testing.T) {
	sub := &fakeDrainer{batches: [][]logwatch.LogEntry{{loopPlanEntry("appdb", 5)}}}
	var mu sync.Mutex
	var stored int
	l := newTestPlanLoop(sub, func(context.Context, autoexplain.ObservedPlan) error {
		mu.Lock()
		stored++
		mu.Unlock()
		return nil
	}, &planLogCapture{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runObservedPlans(ctx, l, 5*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := stored
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runObservedPlans did not return after cancel")
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if stored != 1 || !sub.stopped {
		t.Fatalf("stored %d, stopped %v; want 1 and the subscription stopped", stored,
			sub.stopped)
	}
}

// Gating: no plan subscriber without auto_explain or without a log watcher.
func TestStartObservedPlans_Gating(t *testing.T) {
	for name, rt := range map[string]*databaseRuntime{
		"nil config":       {},
		"auto_explain off": {cfg: &config.Config{}},
		"no log watcher": {cfg: &config.Config{
			AutoExplain: config.AutoExplainConfig{Enabled: true}}},
	} {
		if rt.startObservedPlans() {
			t.Fatalf("%s: plan ingestion started", name)
		}
	}
}

func TestObservedPlanCap(t *testing.T) {
	for in, want := range map[int]int{0: 100, -3: 100, 1: 1, 250: 250} {
		if got := observedPlanCap(in); got != want {
			t.Errorf("observedPlanCap(%d) = %d, want %d", in, got, want)
		}
	}
}
