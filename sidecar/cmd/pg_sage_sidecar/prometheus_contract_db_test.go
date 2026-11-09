package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/shadow"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// State transitions (integration): shadow decision counters are
// process-wide and keyed by database. A fleet reload that removes a
// database prunes its counters; the remaining database keeps its own.
// Before observability-07 the removed database's series stayed until
// restart.
func TestContractFleetReloadPrunesShadowCounters(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "contract_shadow_prune")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := shadow.NewStore(openComposedPool(t, ctx, dsn))
	// Names unique to this test: the counters are shared by the package.
	keep, removed := "contract_keep", "contract_gone"
	for _, db := range []string{keep, removed} {
		recordShadowDecision(t, ctx, store, db)
	}
	installContractFleet(t, "fleet", keep, removed)
	before := lintScrape(t, scrapeMetrics(t))
	for _, db := range []string{keep, removed} {
		if !shadowSeriesFor(before, db) {
			t.Fatalf("fixture: no shadow decision series for %s:\n%s", db,
				grepLines(before.body, "pg_sage_shadow_decisions_total"))
		}
	}
	reloadRemovingDatabase(t, []string{keep, removed}, removed)
	after := lintScrape(t, scrapeMetrics(t))
	if shadowSeriesFor(after, removed) || hasLabelValue(after, removed) {
		t.Errorf("series of the removed database remain:\n%s",
			grepLines(after.body, removed))
	}
	if !shadowSeriesFor(after, keep) {
		t.Errorf("the reload pruned the kept database's shadow counters:\n%s",
			grepLines(after.body, "pg_sage_shadow_decisions_total"))
	}
}

func recordShadowDecision(t *testing.T, ctx context.Context, store *shadow.Store, db string) {
	t.Helper()
	sql := "CREATE INDEX CONCURRENTLY ON public.o (customer_id)"
	family, class := shadow.ClassOf("create_index_concurrently", sql)
	shape := shadow.Shape(sql)
	d := shadow.Decision{Database: db, Fingerprint: shadow.Fingerprint(class, db, shape),
		Family: family, Class: class, Title: "contract", Object: "public.o", SQL: sql,
		Shape: shape, Evidence: map[string]any{}, GateVerdict: "observe_only",
		GateReason: "autonomy_level", TrustedVerdict: "execute",
		TrustedReason: "autonomy_l3", GrantedLevel: 1}
	if _, inserted, err := store.Record(ctx, d); err != nil || !inserted {
		t.Fatalf("record shadow decision for %s: inserted=%t err=%v", db, inserted, err)
	}
}

func shadowSeriesFor(e exposition, db string) bool {
	for _, v := range e.labelValues("pg_sage_shadow_decisions_total", "database") {
		if v == db {
			return true
		}
	}
	return false
}
