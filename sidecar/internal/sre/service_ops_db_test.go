package sre

import (
	"context"
	"errors"
	"testing"
)

// Operator control of investigations (AI-SRE-SPEC §4 R1 "start from a
// Case or by operator", §9 stop/resume, CHECK-24/25): an operator can
// start an investigation of a case (coalesced with a live one of the
// same trigger), stop it (a resumable pause that keeps steps and
// evidence) and resume it, under an If-Match version precondition. Every
// change is attributed to the operator in the event chain.

func opService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return NewService("orders", c, st), ctx
}

func opTrigger() Trigger {
	return Trigger{CaseID: "case:ops:1", Kind: TriggerLock, Subject: "incident ops",
		IdempotencyKey: "ops:1", Actor: "user:7"}
}

func lastEvent(t *testing.T, ctx context.Context, svc *Service, id UUID) Event {
	t.Helper()
	events, verified, err := svc.Events(ctx, id)
	if err != nil || !verified || len(events) == 0 {
		t.Fatalf("events %+v verified %v (%v)", events, verified, err)
	}
	return events[len(events)-1]
}

func TestService_StartByOperatorIsQueuedAndAttributed(t *testing.T) {
	svc, ctx := opService(t)
	inv, created, err := svc.Start(ctx, opTrigger())
	if err != nil || !created || inv.State != StateQueued || inv.TriggerKind != TriggerLock ||
		inv.CaseID != "case:ops:1" {
		t.Fatalf("start = %+v created %v (%v)", inv, created, err)
	}
	if e := lastEvent(t, ctx, svc, inv.ID); e.Type != EventCreated || e.Actor != "user:7" {
		t.Fatalf("created event %+v, want actor user:7", e)
	}
	again, created, err := svc.Start(ctx, opTrigger())
	if err != nil || created || again.ID != inv.ID {
		t.Fatalf("a repeated start must coalesce: %+v created %v (%v)", again, created, err)
	}
}

func TestService_StartValidates(t *testing.T) {
	svc, ctx := opService(t)
	for name, tr := range map[string]Trigger{
		"unknown kind": {CaseID: "c", Kind: "cosmic_rays", Actor: "user:7"},
		"no case":      {Kind: TriggerLock, Actor: "user:7"},
		"no actor":     {CaseID: "c", Kind: TriggerLock},
	} {
		if _, _, err := svc.Start(ctx, tr); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err %v, want ErrInvalidRequest", name, err)
		}
	}
	var nilSvc *Service
	if _, _, err := nilSvc.Start(ctx, opTrigger()); !errors.Is(err,
		ErrMetadataUnavailable) {
		t.Errorf("nil service: %v", err)
	}
}

// CHECK-24: stop is a resumable pause; resume re-queues the same
// investigation, which then runs to its conclusion from its stored steps.
func TestService_StopAndResume(t *testing.T) {
	svc, ctx := opService(t)
	inv, _, err := svc.Start(ctx, opTrigger())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	stopped, err := svc.Stop(ctx, inv.ID, inv.Version, "user:7")
	if err != nil || stopped.State != StatePaused || stopped.Version <= inv.Version {
		t.Fatalf("stop = %+v (%v)", stopped, err)
	}
	if e := lastEvent(t, ctx, svc, inv.ID); e.Type != EventTransition ||
		e.Actor != "user:7" {
		t.Fatalf("stop event %+v, want a transition by user:7", e)
	}
	if _, err := svc.Resume(ctx, inv.ID, inv.Version, "user:7"); !errors.Is(err,
		ErrVersionConflict) {
		t.Fatalf("resume with a stale version: %v, want ErrVersionConflict", err)
	}
	resumed, err := svc.Resume(ctx, inv.ID, stopped.Version, "user:7")
	if err != nil || resumed.State != StateQueued {
		t.Fatalf("resume = %+v (%v)", resumed, err)
	}
	if err := svc.Coordinator().Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate after resume: %v", err)
	}
	d, err := svc.Detail(ctx, inv.ID)
	if err != nil || d.Investigation.State != StateConcluded ||
		d.Investigation.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("after resume: %+v (%v)", d.Investigation, err)
	}
	if _, err := svc.Stop(ctx, inv.ID, d.Investigation.Version, "user:7"); !errors.Is(err,
		ErrInvalidTransition) {
		t.Fatalf("stopping a concluded investigation: %v, want ErrInvalidTransition", err)
	}
}

func TestService_StopResumeValidate(t *testing.T) {
	svc, ctx := opService(t)
	inv, _, err := svc.Start(ctx, opTrigger())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := svc.Stop(ctx, inv.ID, inv.Version, ""); !errors.Is(err,
		ErrInvalidRequest) {
		t.Errorf("stop without an actor: %v", err)
	}
	if _, err := svc.Stop(ctx, NewUUID(), 1, "user:7"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stop of an unknown id: %v", err)
	}
	if _, err := svc.Resume(ctx, inv.ID, inv.Version, "user:7"); !errors.Is(err,
		ErrInvalidTransition) {
		t.Errorf("resuming a queued investigation: %v, want ErrInvalidTransition", err)
	}
	if _, err := svc.Stop(ctx, "not-a-uuid", 1, "user:7"); !errors.Is(err,
		ErrInvalidRequest) {
		t.Errorf("stop of a malformed id: %v", err)
	}
}

// CHECK-09/26: another database's service cannot stop this one's work.
func TestService_StopIsScopedToTheDatabase(t *testing.T) {
	svc, ctx := opService(t)
	inv, _, err := svc.Start(ctx, opTrigger())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	other, _ := opService(t)
	if _, err := other.Stop(ctx, inv.ID, inv.Version, "user:7"); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("cross-database stop: %v, want ErrNotFound", err)
	}
	got, err := svc.Detail(ctx, inv.ID)
	if err != nil || got.Investigation.State != StateQueued {
		t.Fatalf("the investigation changed: %+v (%v)", got.Investigation, err)
	}
}

// The trigger loop keeps its own attribution.
func TestCoordinator_TriggerStartIsAttributedToTheTrigger(t *testing.T) {
	svc, ctx := opService(t)
	tr := opTrigger()
	tr.Actor = ""
	inv, _, err := svc.Coordinator().Start(ctx, tr)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if e := lastEvent(t, ctx, svc, inv.ID); e.Actor != "trigger" {
		t.Fatalf("created event actor %q, want trigger", e.Actor)
	}
}

// Post-test audit: a resumed investigation is handed to the worker queue
// (the loop's next scan would also find it, but resume should not wait).
func TestService_ResumeQueuesTheInvestigation(t *testing.T) {
	svc, ctx := opService(t)
	inv, _, err := svc.Start(ctx, opTrigger())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-svc.Coordinator().queue // Start queued it
	stopped, err := svc.Stop(ctx, inv.ID, inv.Version, "user:7")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := svc.Resume(ctx, inv.ID, stopped.Version, "user:7"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case id := <-svc.Coordinator().queue:
		if id != inv.ID {
			t.Fatalf("queued %s, want %s", id, inv.ID)
		}
	default:
		t.Fatal("resume did not queue the investigation")
	}
}
