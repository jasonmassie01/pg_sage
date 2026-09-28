package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M2 exit path: collector -> lock-chain fast path -> RCA
// incident -> investigation trigger -> probe plan -> causal diagnosis ->
// persisted, case-linked investigation, all against real PostgreSQL and
// through the constructor every runtime mode uses.
func TestComposedSRE_M2_IncidentBecomesAConcludedInvestigation(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	c := composedConfig("standalone")
	c.SRE.AutomaticStart = true
	installComposedGlobals(t, c)
	pool := openComposedPool(t, ctx, dsn)
	rt := startComposedRuntime(t, ctx, "m2_db", pool, pool, nil)
	rt.slowCycle(t, ctx)
	chain := startComposedChain(t, ctx, dsn, pool)
	rt.fastTick(t, ctx)
	var incidentID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM sage.incidents
		WHERE database_name = 'm2_db' AND resolved_at IS NULL
		  AND signal_ids = ARRAY['lock_contention']`).Scan(&incidentID); err != nil {
		t.Fatalf("lock incident: %v", err)
	}

	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: rt.adapter.probes, name: "m2_db",
		runtimeKey: fmt.Sprintf("composed:m2:%d", time.Now().UnixNano()),
		settings:   c.SRE, logFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.Coordinator().Run(runCtx); close(done) }()
	t.Cleanup(func() { stop(); <-done })

	inv := waitInvestigation(t, ctx, svc, incidentID)
	if inv.State != sre.StateConcluded || inv.Summary.Root != "idle_in_tx_holder" ||
		inv.Summary.Subject != fmt.Sprintf("pid %d", chain.holderPID) {
		t.Fatalf("investigation = %+v, want the idle holder pid %d", inv, chain.holderPID)
	}
	wantCase := cases.IncidentIdentityKey(cases.SourceIncident{ID: incidentID,
		DatabaseName: "m2_db", SignalIDs: []string{"lock_contention"},
		Source: "deterministic"})
	if inv.CaseID != wantCase || inv.TriggerKind != sre.TriggerLock {
		t.Fatalf("case link %q (%s), want %q", inv.CaseID, inv.TriggerKind, wantCase)
	}
}

func waitInvestigation(t *testing.T, ctx context.Context, svc *sre.Service,
	incidentID string) sre.Investigation {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		page, err := svc.List(ctx, sre.ListFilter{})
		if err == nil {
			for _, inv := range page.Items {
				if inv.IncidentID == incidentID && inv.State.Terminal() {
					return inv
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no finished investigation for incident %s", incidentID)
	return sre.Investigation{}
}
