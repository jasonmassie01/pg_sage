package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// config-04c review: the gauge reads 0 once a builder finished (it is not
// removed), and a replaced runtime's builder never drops its successor's
// series.

// fakeBuilds replaces the builder with one that reports counts and returns.
func fakeBuilds(t *testing.T, counts schema.DeferredIndexCounts) {
	t.Helper()
	saved := runDeferredIndexBuilder
	t.Cleanup(func() { runDeferredIndexBuilder = saved })
	runDeferredIndexBuilder = func(_ context.Context, b *schema.DeferredIndexBuilder) {
		b.Observe(counts)
	}
}

func waitWorkers(t *testing.T, workers *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the builder worker did not stop")
	}
}

// waitGauge polls until the database's series is present (want true) or
// gone (want false).
func waitGauge(t *testing.T, name string, want bool) schema.DeferredIndexCounts {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, ok := deferredIndexGauges.snapshot()[name]
		if ok == want {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("gauge for %s present=%v, want %v", name, ok, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// State transition: a builder that built everything returns; its zero
// counts stay scraped until the database goes.
func TestDeferredIndexBuilder_FinishedBuilderReadsZero(t *testing.T) {
	fakeBuilds(t, schema.DeferredIndexCounts{})
	rt := fakeBuilderRuntime("finished1")
	rt.startDeferredIndexBuilder()
	if c := waitGauge(t, "finished1", true); c != (schema.DeferredIndexCounts{}) {
		t.Fatalf("gauge after the build = %+v, want zero", c)
	}
	time.Sleep(50 * time.Millisecond) // the builder has returned by now
	if _, ok := deferredIndexGauges.snapshot()["finished1"]; !ok {
		t.Fatal("a finished builder removed its gauge; it must read 0")
	}
	rt.cancel()
	waitWorkers(t, rt.workers)
	waitGauge(t, "finished1", false)
}

// Concurrency (fleet rebuild): the old runtime ends after its successor
// reported; the successor's series stays.
func TestDeferredIndexBuilder_ReplacedRuntimeKeepsSuccessorGauge(t *testing.T) {
	fakeBuilds(t, schema.DeferredIndexCounts{Missing: 1})
	old := fakeBuilderRuntime("rebuilt1")
	old.startDeferredIndexBuilder()
	waitGauge(t, "rebuilt1", true)
	saved := runDeferredIndexBuilder
	runDeferredIndexBuilder = func(_ context.Context, b *schema.DeferredIndexBuilder) {
		b.Observe(schema.DeferredIndexCounts{Missing: 3})
	}
	successor := fakeBuilderRuntime("rebuilt1")
	successor.startDeferredIndexBuilder()
	deadline := time.Now().Add(5 * time.Second)
	for deferredIndexGauges.snapshot()["rebuilt1"].Missing != 3 {
		if time.Now().After(deadline) {
			t.Fatal("the successor's builder did not report")
		}
		time.Sleep(5 * time.Millisecond)
	}
	runDeferredIndexBuilder = saved
	old.cancel()
	waitWorkers(t, old.workers)
	if c, ok := deferredIndexGauges.snapshot()["rebuilt1"]; !ok || c.Missing != 3 {
		t.Fatalf("after the old runtime ended: gauge %+v present %v, want the successor's "+
			"missing=3", c, ok)
	}
	successor.cancel()
	waitWorkers(t, successor.workers)
	waitGauge(t, "rebuilt1", false)
}

// Integration (shutdown): the meta database's builder is tracked, so the
// shutdown can wait for it; ending the context ends it and its series.
func TestMetaDeferredIndexBuilder_ShutdownWaitsForIt(t *testing.T) {
	fakeBuilds(t, schema.DeferredIndexCounts{Invalid: 2})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startMetaDeferredIndexBuilder(ctx, &pgxpool.Pool{})
	if c := waitGauge(t, "meta-db", true); c.Invalid != 2 {
		t.Fatalf("meta gauge = %+v, want invalid=2", c)
	}
	cancel()
	waitWorkers(t, &metaDeferredIndexWorkers)
	if _, ok := deferredIndexGauges.snapshot()["meta-db"]; ok {
		t.Fatal("the meta builder kept its gauge after shutdown")
	}
}

// Nil input: no meta pool, no builder and no gauge.
func TestMetaDeferredIndexBuilder_NilPoolStartsNothing(t *testing.T) {
	called := false
	saved := runDeferredIndexBuilder
	t.Cleanup(func() { runDeferredIndexBuilder = saved })
	runDeferredIndexBuilder = func(context.Context, *schema.DeferredIndexBuilder) { called = true }
	startMetaDeferredIndexBuilder(context.Background(), nil)
	waitWorkers(t, &metaDeferredIndexWorkers)
	if called {
		t.Fatal("a nil meta pool started a builder")
	}
}
