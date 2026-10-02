package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M4 (R1 GA): with the shipped defaults (no sre section at
// all) an open lock incident starts a read-only investigation by
// itself, with no LLM configured (CHECK-33: R1 operates with the LLM
// off). sre.automatic_start: false starts nothing, but the loop still
// runs (pending work and retention).

func TestComposedSRE_M4_DefaultsAutoStartWithoutLLM(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	c := composedConfig("standalone")
	if !c.SRE.AutomaticStart {
		t.Fatal("the default config does not start investigations automatically")
	}
	installComposedGlobals(t, c)
	pool := openComposedPool(t, ctx, dsn)
	rt := startComposedRuntime(t, ctx, "m4_auto_db", pool, pool, nil)
	rt.slowCycle(t, ctx)
	chain := startComposedChain(t, ctx, dsn, pool)
	rt.fastTick(t, ctx)
	incidentID := openLockIncident(t, ctx, rt)

	svc := runComposedInvestigator(t, ctx, rt, c.SRE.AutomaticStart, "m4:auto")
	inv := waitInvestigation(t, ctx, svc, incidentID)
	if inv.State != sre.StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.Summary.Subject != fmt.Sprintf("pid %d", chain.holderPID) {
		t.Fatalf("auto-started investigation = %+v, want the idle holder pid %d", inv,
			chain.holderPID)
	}
	if inv.ModelTurns != 0 || inv.Summary.ModelRanking != nil || inv.Summary.Narrative != nil {
		t.Fatalf("no LLM is configured, yet the investigation has model output: %+v", inv)
	}
}

func TestComposedSRE_M4_ExplicitFalseStartsNothing(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	c := composedConfig("standalone")
	c.SRE.AutomaticStart = false
	installComposedGlobals(t, c)
	pool := openComposedPool(t, ctx, dsn)
	rt := startComposedRuntime(t, ctx, "m4_off_db", pool, pool, nil)
	rt.slowCycle(t, ctx)
	startComposedChain(t, ctx, dsn, pool)
	rt.fastTick(t, ctx)
	incidentID := openLockIncident(t, ctx, rt)

	svc := runComposedInvestigator(t, ctx, rt, c.SRE.AutomaticStart, "m4:off")
	// The loop's first tick polls triggers immediately when automatic
	// start is on; give it several chances to (wrongly) start one.
	time.Sleep(3 * time.Second)
	page, err := svc.List(ctx, sre.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, inv := range page.Items {
		if inv.IncidentID == incidentID {
			t.Fatalf("automatic_start: false started investigation %s", inv.ID)
		}
	}
	if len(page.Items) != 0 {
		t.Fatalf("automatic_start: false left %d investigations", len(page.Items))
	}
}

// openLockIncident returns the runtime's one open lock incident.
func openLockIncident(t *testing.T, ctx context.Context, rt *composedRuntime) string {
	t.Helper()
	var id string
	if err := rt.pool.QueryRow(ctx, `SELECT id::text FROM sage.incidents
		WHERE database_name = $1 AND resolved_at IS NULL
		  AND signal_ids = ARRAY['lock_contention']`, rt.name).Scan(&id); err != nil {
		t.Fatalf("%s lock incident: %v", rt.name, err)
	}
	return id
}

// runComposedInvestigator runs the runtime's investigator loop (no LLM)
// until the test ends.
func runComposedInvestigator(t *testing.T, ctx context.Context, rt *composedRuntime,
	automatic bool, key string) *sre.Service {
	t.Helper()
	settings := cfg.SRE
	settings.AutomaticStart = automatic
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: rt.pool,
		monitored: rt.pool, runner: rt.adapter.probes, name: rt.name,
		runtimeKey: fmt.Sprintf("composed:%s:%d", key, time.Now().UnixNano()),
		settings:   settings, notices: &sre.OnceLog{},
		logFn:      func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.Coordinator().Run(runCtx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	return svc
}
