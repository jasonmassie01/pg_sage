package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
)

// config-04c wiring: each database runtime runs one background index
// builder under its own lifecycle; removing or rebuilding the database
// cancels it and drops its gauge. The builder itself is tested in
// internal/schema on a real server.

func fakeBuilderRuntime(name string) *databaseRuntime {
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: name}, workers: &sync.WaitGroup{}}
	rt.ctx, rt.cancel = context.WithCancel(context.Background())
	return rt
}

// State transition (meta-DB member removal, fleet rebuild): cancelling
// the runtime cancels its builder and drops the database's gauge.
func TestDeferredIndexBuilder_RuntimeCancelStopsIt(t *testing.T) {
	saved := runDeferredIndexBuilder
	t.Cleanup(func() { runDeferredIndexBuilder = saved })
	started := make(chan *schema.DeferredIndexBuilder, 1)
	runDeferredIndexBuilder = func(ctx context.Context, b *schema.DeferredIndexBuilder) {
		b.Observe(schema.DeferredIndexCounts{Missing: 2})
		started <- b
		<-ctx.Done()
	}
	rt := fakeBuilderRuntime("member1")
	rt.startDeferredIndexBuilder()
	var b *schema.DeferredIndexBuilder
	select {
	case b = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the builder did not start")
	}
	if b.Database != "member1" || b.Stopped == nil || b.Observe == nil {
		t.Fatalf("builder %+v, want database member1 with a stop check and a gauge", b)
	}
	if got := deferredIndexGauges.snapshot()["member1"]; got.Missing != 2 {
		t.Fatalf("gauge while running = %+v, want missing=2", got)
	}
	rt.cancel()
	done := make(chan struct{})
	go func() { rt.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the runtime did not stop its builder")
	}
	if _, ok := deferredIndexGauges.snapshot()["member1"]; ok {
		t.Fatal("a removed database keeps its deferred index gauge")
	}
}

// The emergency stop predicate reads the manager's latch; without a
// manager or an instance (startup, the meta database) it never stops.
func TestDeferredBuildStopped(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	inst := &fleet.DatabaseInstance{Name: "db1"}
	if deferredBuildStopped(mgr, inst) {
		t.Fatal("stopped without an emergency stop")
	}
	inst.Stopped = true
	if !deferredBuildStopped(mgr, inst) {
		t.Fatal("not stopped during an emergency stop")
	}
	if deferredBuildStopped(nil, inst) || deferredBuildStopped(mgr, nil) {
		t.Fatal("nil manager or instance reported a stop")
	}
}

func TestWriteDeferredIndexMetrics(t *testing.T) {
	g := &deferredIndexGaugeStore{}
	var empty strings.Builder
	writeDeferredIndexMetrics(&empty, g)
	if empty.Len() != 0 {
		t.Fatalf("no builder wrote %q, want nothing", empty.String())
	}
	g.set("db2", nil, schema.DeferredIndexCounts{})
	g.set("db1", nil, schema.DeferredIndexCounts{Missing: 1, Building: 3, Invalid: 2})
	var b strings.Builder
	writeDeferredIndexMetrics(&b, g)
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_schema_deferred_indexes gauge",
		`pg_sage_schema_deferred_indexes{database="db1",state="missing"} 1`,
		`pg_sage_schema_deferred_indexes{database="db1",state="building"} 3`,
		`pg_sage_schema_deferred_indexes{database="db1",state="invalid"} 2`,
		`pg_sage_schema_deferred_indexes{database="db2",state="missing"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Index(out, `database="db1"`) > strings.Index(out, `database="db2"`) {
		t.Errorf("databases are not sorted:\n%s", out)
	}
	g.drop("db1", nil)
	if _, ok := g.snapshot()["db1"]; ok {
		t.Fatal("drop kept db1")
	}
}

// Concurrent access: builders of many databases report at once.
func TestDeferredIndexGaugeStore_Concurrent(t *testing.T) {
	g := &deferredIndexGaugeStore{}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := string(rune('a' + i))
			g.set(name, nil, schema.DeferredIndexCounts{Missing: i})
			var b strings.Builder
			writeDeferredIndexMetrics(&b, g)
		}()
	}
	wg.Wait()
	if n := len(g.snapshot()); n != 16 {
		t.Fatalf("store holds %d databases, want 16", n)
	}
}
