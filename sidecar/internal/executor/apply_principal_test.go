package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Cross-database re-check (AGENTDB-SPEC §6.2.7): an agent-originated
// change holds its principal active in the control database from the
// re-authorization until the change (and its target COMMIT) returns.

type holdProbe struct {
	mu      sync.Mutex
	events  []string
	ids     []string
	err     error
	holding atomic.Int32
}

func (h *holdProbe) hold(_ context.Context, id string) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "hold")
	h.ids = append(h.ids, id)
	if h.err != nil {
		return nil, h.err
	}
	h.holding.Add(1)
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, "release")
		h.holding.Add(-1)
	}, nil
}

func (h *holdProbe) note(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

func (h *holdProbe) snapshot() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...), append([]string(nil), h.ids...)
}

func agentIntent(h *holdProbe, executeErr error) ActionIntent {
	return ActionIntent{
		Request: policy.ActionRequest{SQL: "ANALYZE public.orders",
			Principal: &policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa"}},
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			h.note("execute")
			return 7, executeErr
		},
	}
}

func TestApplyHoldsAgentPrincipalAroundExecution(t *testing.T) {
	e, gate := applyExecutor(30, 0)
	h := &holdProbe{}
	e.WithPrincipalHold(h.hold)
	id, err := e.Apply(context.Background(), agentIntent(h, nil))
	if err != nil || id != 7 {
		t.Fatalf("Apply = (%d, %v), want (7, nil)", id, err)
	}
	events, ids := h.snapshot()
	if want := []string{"hold", "execute", "release"}; !equalStrings(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if len(ids) != 1 || ids[0] != "agp_aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("held ids = %v", ids)
	}
	if len(gate.remaining) != 2 {
		t.Fatalf("authorizations = %d, want 2 (the hold follows the re-authorization)",
			len(gate.remaining))
	}
}

func TestApplyReleasesHoldWhenExecutionFails(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	h := &holdProbe{}
	e.WithPrincipalHold(h.hold)
	boom := errors.New("target commit failed")
	if _, err := e.Apply(context.Background(), agentIntent(h, boom)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the execution error", err)
	}
	if h.holding.Load() != 0 {
		t.Fatal("the hold leaked after a failed execution")
	}
}

func TestApplyInactivePrincipalStopsBeforeExecution(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	frozen := errors.New("agentguard: denied: agent_frozen")
	h := &holdProbe{err: frozen}
	e.WithPrincipalHold(h.hold)
	_, err := e.Apply(context.Background(), agentIntent(h, nil))
	if !errors.Is(err, ErrPrincipalInactive) || !errors.Is(err, frozen) {
		t.Fatalf("err = %v, want ErrPrincipalInactive wrapping the denial", err)
	}
	if events, _ := h.snapshot(); !equalStrings(events, []string{"hold"}) {
		t.Fatalf("events = %v, want no execution", events)
	}
}

func TestApplyHoldsContextPrincipal(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	h := &holdProbe{}
	e.WithPrincipalHold(h.hold)
	intent := agentIntent(h, nil)
	intent.Request.Principal = nil
	ctx := policy.WithPrincipalRef(context.Background(),
		policy.PrincipalRef{ID: "agp_bbbbbbbbbbbbbbbbbbbb"})
	if _, err := e.Apply(ctx, intent); err != nil {
		t.Fatalf("Apply = %v", err)
	}
	if _, ids := h.snapshot(); len(ids) != 1 || ids[0] != "agp_bbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("held = %v, want the context principal", ids)
	}
}

func TestApplyWithoutPrincipalTakesNoHold(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	h := &holdProbe{}
	e.WithPrincipalHold(h.hold)
	intent := agentIntent(h, nil)
	intent.Request.Principal = nil
	if _, err := e.Apply(context.Background(), intent); err != nil {
		t.Fatalf("Apply = %v", err)
	}
	if events, _ := h.snapshot(); !equalStrings(events, []string{"execute"}) {
		t.Fatalf("events = %v, want no hold for pg_sage's own change", events)
	}
}

func TestApplyNarrowingTakesNoHold(t *testing.T) {
	// The kill takes FOR UPDATE on the principal: a narrowing step holding
	// FOR SHARE would wait on (or block) the containment it is part of.
	e, _ := applyExecutor(30, 0)
	h := &holdProbe{err: errors.New("must not be called")}
	e.WithPrincipalHold(h.hold)
	intent := agentIntent(h, nil)
	intent.Request.Contract = &policy.ActionContract{ActionType: "guard_revoke",
		RiskTier: policy.RiskSafe, Narrowing: true}
	if _, err := e.Apply(context.Background(), intent); err != nil {
		t.Fatalf("Apply = %v", err)
	}
	if events, _ := h.snapshot(); !equalStrings(events, []string{"execute"}) {
		t.Fatalf("events = %v, want no hold for a narrowing contract", events)
	}
}

func TestApplyAgentChangeWithoutHoldFailsClosed(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	h := &holdProbe{}
	_, err := e.Apply(context.Background(), agentIntent(h, nil))
	if !errors.Is(err, ErrPrincipalInactive) {
		t.Fatalf("err = %v, want ErrPrincipalInactive without a configured hold", err)
	}
	if events, _ := h.snapshot(); len(events) != 0 {
		t.Fatalf("events = %v, want nothing executed", events)
	}
}

func TestApplyConcurrentAgentHoldsBalance(t *testing.T) {
	e, _ := applyExecutor(30, 0)
	h := &holdProbe{}
	e.WithPrincipalHold(h.hold)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Apply(context.Background(), agentIntent(h, nil)); err != nil {
				t.Errorf("Apply = %v", err)
			}
		}()
	}
	wg.Wait()
	events, _ := h.snapshot()
	holds, releases := 0, 0
	for _, ev := range events {
		switch ev {
		case "hold":
			holds++
		case "release":
			releases++
		}
	}
	if holds != 16 || releases != 16 || h.holding.Load() != 0 {
		t.Fatalf("holds=%d releases=%d open=%d, want 16/16/0", holds, releases,
			h.holding.Load())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
