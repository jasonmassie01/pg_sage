package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-06: registration starts the log fast path next to the lock-chain
// fast path when the database has a log source.

type stubCriticalSource struct{}

func (stubCriticalSource) DrainCritical() []*rca.Signal { return nil }

func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRCAAdapterLogFastPathLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := unreachablePool(t)
	noLog := func(string, string, ...any) {}
	var workers sync.WaitGroup

	on := rcaAdapterTestConfig()
	on.RCA.LogFastPathIntervalSeconds = 5
	a := newRCAAdapter(rcaAdapterDeps{ctx: ctx, eng: rca.NewEngine(&on.RCA, noLog),
		pool: pool, name: "db", cfg: on, logFn: noLog, workers: &workers,
		logSource: stubCriticalSource{}})
	if a.logFastPath == nil || a.logFastPath.Interval() != 5*time.Second {
		t.Fatalf("log fast path = %v, want started at 5s", a.logFastPath)
	}
	if a.fastPath != nil {
		t.Fatal("lock-chain fast path started with its interval at 0")
	}

	noSource := newRCAAdapter(rcaAdapterDeps{ctx: ctx, eng: rca.NewEngine(&on.RCA, noLog),
		pool: pool, name: "db", cfg: on, logFn: noLog, workers: &workers})
	if noSource.logFastPath != nil {
		t.Fatal("no log source must leave the log fast path off")
	}

	off := rcaAdapterTestConfig() // log_fast_path_interval_seconds: 0
	b := newRCAAdapter(rcaAdapterDeps{ctx: ctx, eng: rca.NewEngine(&off.RCA, noLog),
		pool: pool, name: "db", cfg: off, logFn: noLog, workers: &workers,
		logSource: stubCriticalSource{}})
	if b.logFastPath != nil {
		t.Fatal("interval 0 must leave the log fast path disabled")
	}

	noPool := newRCAAdapter(rcaAdapterDeps{ctx: ctx, eng: rca.NewEngine(&on.RCA, noLog),
		name: "db", cfg: on, logFn: noLog, workers: &workers,
		logSource: stubCriticalSource{}})
	if noPool.logFastPath != nil {
		t.Fatal("no incident store must leave the log fast path off")
	}
}

// The log fast-path goroutine belongs to the instance worker group and
// stops with the instance context.
func TestRCAAdapterLogFastPathIsTrackedByWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := unreachablePool(t)
	noLog := func(string, string, ...any) {}
	c := rcaAdapterTestConfig()
	c.RCA.LogFastPathIntervalSeconds = 5
	var workers sync.WaitGroup
	newRCAAdapter(rcaAdapterDeps{ctx: ctx, eng: rca.NewEngine(&c.RCA, noLog),
		pool: pool, name: "db", cfg: c, logFn: noLog, workers: &workers,
		logSource: stubCriticalSource{}})
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("the worker group did not track the log fast path")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the log fast path did not stop with the instance context")
	}
}
