package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/rca"
)

// migration-05: the database's lock-chain observer nudges the RCA fast
// path, so a tick runs at once instead of a whole interval later.
func TestRCAAdapterLockObserverNudgesFastPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx,
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(pool.Close)
	var mu sync.Mutex
	var lines []string
	logFn := func(level, msg string, _ ...any) {
		mu.Lock()
		lines = append(lines, level+" "+msg)
		mu.Unlock()
	}
	c := rcaAdapterTestConfig()
	c.RCA.LockChainIntervalSeconds = 3600
	obs := analyzer.NewLockChainObserver(c.RCA.LockChainInterval())
	var workers sync.WaitGroup
	a := newRCAAdapter(rcaAdapterDeps{ctx: ctx, eng: rca.NewEngine(&c.RCA, logFn),
		pool: pool, name: "db", cfg: c, logFn: logFn, workers: &workers, lockObs: obs})
	if a.fastPath == nil {
		t.Fatal("fast path not started")
	}
	time.Sleep(50 * time.Millisecond) // let Run start its ticker
	obs.Nudge()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, l := range lines {
			if strings.HasPrefix(l, "WARN rca: lock-chain fast path") {
				mu.Unlock()
				return // the nudged tick ran (and failed on the dead pool)
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nudge ran no fast-path tick within 5s (interval 1h): %v", lines)
}
