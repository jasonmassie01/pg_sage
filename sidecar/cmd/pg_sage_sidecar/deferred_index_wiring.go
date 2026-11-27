package main

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/schema"
)

// Background index builder wiring (config-04c). Every database whose sage
// schema a boot prepared (standalone, each YAML fleet database, each
// meta-db member, and the meta database itself) gets one builder under its
// own lifecycle: removing or rebuilding a fleet member cancels its builder,
// which cancels the running build on the server.

// runDeferredIndexBuilder runs a builder; tests replace it.
var runDeferredIndexBuilder = func(ctx context.Context, b *schema.DeferredIndexBuilder) {
	b.Run(ctx)
}

// deferredIndexGauges backs pg_sage_schema_deferred_indexes.
var deferredIndexGauges = &deferredIndexGaugeStore{}

// deferredIndexGaugeStore holds each database's latest counts and the
// builder that wrote them, so a replaced runtime's builder, ending after
// its successor started, never drops the successor's series.
type deferredIndexGaugeStore struct {
	mu     sync.Mutex
	counts map[string]deferredIndexGauge
}

type deferredIndexGauge struct {
	owner  *schema.DeferredIndexBuilder
	counts schema.DeferredIndexCounts
}

func (g *deferredIndexGaugeStore) set(database string, owner *schema.DeferredIndexBuilder,
	c schema.DeferredIndexCounts) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.counts == nil {
		g.counts = map[string]deferredIndexGauge{}
	}
	g.counts[database] = deferredIndexGauge{owner: owner, counts: c}
}

// drop removes the database's series when owner wrote it last.
func (g *deferredIndexGaugeStore) drop(database string, owner *schema.DeferredIndexBuilder) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.counts[database]; ok && cur.owner == owner {
		delete(g.counts, database)
	}
}

func (g *deferredIndexGaugeStore) snapshot() map[string]schema.DeferredIndexCounts {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]schema.DeferredIndexCounts, len(g.counts))
	for name, cur := range g.counts {
		out[name] = cur.counts
	}
	return out
}

// runDeferredIndexBuilderFor runs b, then keeps its last counts (0 once
// every index is built) until ctx ends, when the database's series goes.
func runDeferredIndexBuilderFor(ctx context.Context, b *schema.DeferredIndexBuilder) {
	defer deferredIndexGauges.drop(b.Database, b)
	runDeferredIndexBuilder(ctx, b)
	<-ctx.Done()
}

// startDeferredIndexBuilder runs the database's builder as an instance
// worker: rt.ctx ends when the database is removed or rebuilt. It asks the
// manager the runtime is built under, read here and never again from the
// builder's goroutine (env-race).
func (rt *databaseRuntime) startDeferredIndexBuilder() {
	name := rt.spec.Name
	mgr, inst := fleetMgr, rt.inst
	b := &schema.DeferredIndexBuilder{
		Pool: rt.spec.Pool, Database: name,
		Stopped: func() bool { return deferredBuildStopped(mgr, inst) },
	}
	b.Observe = func(c schema.DeferredIndexCounts) { deferredIndexGauges.set(name, b, c) }
	ctx := rt.ctx
	rt.start(func() { runDeferredIndexBuilderFor(ctx, b) })
}

// metaDeferredIndexWorkers tracks the meta database's builder, so the
// shutdown waits for it to cancel its running build on the server.
var metaDeferredIndexWorkers sync.WaitGroup

// startMetaDeferredIndexBuilder runs the meta database's builder for the
// life of the process. The meta database has no emergency stop.
func startMetaDeferredIndexBuilder(ctx context.Context, metaPool *pgxpool.Pool) {
	if metaPool == nil || ctx == nil {
		return
	}
	b := &schema.DeferredIndexBuilder{Pool: metaPool, Database: "meta-db"}
	b.Observe = func(c schema.DeferredIndexCounts) { deferredIndexGauges.set(b.Database, b, c) }
	startInstanceWorker(&metaDeferredIndexWorkers, func() { runDeferredIndexBuilderFor(ctx, b) })
}

// deferredBuildStopped reports the database's emergency stop; without a
// manager or an instance (yet) nothing is stopped.
func deferredBuildStopped(mgr *fleet.DatabaseManager, inst *fleet.DatabaseInstance) bool {
	return mgr != nil && inst != nil && mgr.InstanceStopped(inst)
}

// writeDeferredIndexMetrics renders the gauge for every database with a
// builder, sorted by database.
func writeDeferredIndexMetrics(b *strings.Builder, g *deferredIndexGaugeStore) {
	counts := g.snapshot()
	if len(counts) == 0 {
		return
	}
	const family = "pg_sage_schema_deferred_indexes"
	metricsreg.Header(b, family)
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		c := counts[name]
		for _, s := range []struct {
			state string
			n     int
		}{{"missing", c.Missing}, {"building", c.Building}, {"invalid", c.Invalid}} {
			writeSample(b, family, withLabel(dbLabel(name), "state", s.state), s.n)
		}
	}
	b.WriteString("\n")
}
