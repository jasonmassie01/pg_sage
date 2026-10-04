package sre

import (
	"errors"
	"strings"
	"testing"
)

// Roadmap 2.4 against real PostgreSQL: a finished investigation that an
// operator refuted exports as a valid replay case through the service
// (API) and through the store alone (CLI, which finds the
// investigation's scope by its id).

func TestExportReplayCase_RefutedInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), nil)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-export"))
	svc := NewService("orders", c, st)
	if _, err := svc.ExportReplayCase(ctx, inv.ID, ReplayExportOptions{}); !errors.Is(err,
		ErrNotContested) {
		t.Fatalf("export before any outcome: err = %v, want ErrNotContested", err)
	}
	if _, err := svc.RecordOutcome(ctx, inv.ID, OutcomeRequest{
		Verdict: string(OutcomeRefuted), ActualNode: "ddl_lock_queue",
		Actor: "operator"}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	exp, err := svc.ExportReplayCase(ctx, inv.ID, ReplayExportOptions{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	cs := validCase(t, exp)
	if cs.Gold.Root != "ddl_lock_queue" || len(cs.Observations) == 0 ||
		!exp.GraphRootPreserved || exp.Contest.GraphRoot != "idle_in_tx_holder" {
		t.Fatalf("case gold %+v, %d observations, preserved %v, contest %+v", cs.Gold,
			len(cs.Observations), exp.GraphRootPreserved, exp.Contest)
	}
	scope, err := st.LookupScope(ctx, inv.ID)
	if err != nil || scope != inv.Scope {
		t.Fatalf("lookup scope = %+v (%v), want %+v", scope, err, inv.Scope)
	}
	viaStore, err := ExportReplayCaseFromStore(ctx, st, scope, inv.ID,
		ReplayExportOptions{KeepIdentifiers: true})
	if err != nil || validCase(t, viaStore).Gold.Root != "ddl_lock_queue" {
		t.Fatalf("store export: %v", err)
	}
	if !strings.Contains(string(mustJSON(t, viaStore)), "public.orders") {
		t.Fatal("an opted-in export keeps the identifiers")
	}
}

func TestExportReplayCase_LatestOutcomeDecides(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), nil)
	inv := startAndRun(t, ctx, c, lockTrigger("w3a-latest"))
	svc := NewService("orders", c, st)
	for _, v := range []string{string(OutcomeRefuted), string(OutcomeConfirmed)} {
		if _, err := svc.RecordOutcome(ctx, inv.ID, OutcomeRequest{Verdict: v,
			Actor: "operator"}); err != nil {
			t.Fatalf("record %s: %v", v, err)
		}
	}
	got, err := st.LatestOutcome(ctx, inv.Scope, inv.ID)
	if err != nil || got == nil || got.Verdict != OutcomeConfirmed {
		t.Fatalf("latest outcome = %+v (%v)", got, err)
	}
	if _, err := svc.ExportReplayCase(ctx, inv.ID, ReplayExportOptions{}); !errors.Is(err,
		ErrNotContested) {
		t.Fatalf("a re-confirmed investigation is no longer contested: %v", err)
	}
}

func TestExportReplayCase_UnknownInvestigation(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	if _, err := st.LookupScope(ctx, NewUUID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup of an unknown id: err = %v, want ErrNotFound", err)
	}
	if _, err := st.LookupScope(ctx, UUID("not-a-uuid")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("lookup of a malformed id: err = %v, want ErrInvalidRequest", err)
	}
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), nil)
	if _, err := NewService("orders", c, st).ExportReplayCase(ctx, NewUUID(),
		ReplayExportOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("export of an unknown id: err = %v, want ErrNotFound", err)
	}
}
