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

type deferredIndexGaugeStore struct {
	mu     sync.Mutex
	counts map[string]schema.DeferredIndexCounts
}

func (g *deferredIndexGaugeStore) set(database string, c schema.DeferredIndexCounts) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.counts == nil {
		g.counts = map[string]schema.DeferredIndexCounts{}
	}
	g.counts[database] = c
}

func (g *deferredIndexGaugeStore) drop(database string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.counts, database)
}

func (g *deferredIndexGaugeStore) snapshot() map[string]schema.DeferredIndexCounts {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.counts)
}

// startDeferredIndexBuilder runs the database's builder as an instance
// worker: rt.ctx ends when the database is removed or rebuilt.
func (rt *databaseRuntime) startDeferredIndexBuilder() {
	name := rt.spec.Name
	b := &schema.DeferredIndexBuilder{
		Pool: rt.spec.Pool, Database: name,
		Stopped: func() bool { return deferredBuildStopped(fleetMgr, rt.inst) },
		Observe: func(c schema.DeferredIndexCounts) { deferredIndexGauges.set(name, c) },
	}
	ctx := rt.ctx
	rt.start(func() {
		defer deferredIndexGauges.drop(name)
		runDeferredIndexBuilder(ctx, b)
	})
}

// startMetaDeferredIndexBuilder runs the meta database's builder for the
// life of the process. The meta database has no emergency stop.
func startMetaDeferredIndexBuilder(ctx context.Context, metaPool *pgxpool.Pool) {
	if metaPool == nil || ctx == nil {
		return
	}
	const name = "meta-db"
	b := &schema.DeferredIndexBuilder{
		Pool: metaPool, Database: name,
		Observe: func(c schema.DeferredIndexCounts) { deferredIndexGauges.set(name, c) },
	}
	go func() {
		defer deferredIndexGauges.drop(name)
		runDeferredIndexBuilder(ctx, b)
	}()
}

// deferredBuildStopped reports the database's emergency stop; without a
// manager or an instance (yet) nothing is stopped.
func deferredBuildStopped(mgr *fleet.DatabaseManager, inst *fleet.DatabaseInstance) bool {
	return mgr != nil && inst != nil && mgr.InstanceStopped(inst)
}

// writeDeferredIndexMetrics renders the gauge for every database with a
// running builder, sorted by database.
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
