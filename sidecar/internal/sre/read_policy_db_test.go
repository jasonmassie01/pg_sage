package sre

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func TestReadPolicyPinnedCreationAndResumeDenial(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	runner := newScriptedRunner()
	policy := &probes.ReadPolicy{Version: 1, Scope: "database", Database: "owned", Binding: "binding-a"}
	cfg := DefaultCoordinatorConfig("policy:" + string(NewUUID()))
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: cfg, ReadPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := c.Start(ctx, lockTrigger("pinned-policy"))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := st.InvestigationReadPolicy(ctx, inv.Scope, inv.ID)
	if err != nil || pinned.Hash() != policy.Hash() {
		t.Fatalf("snapshot=%+v err=%v", pinned, err)
	}
	// Widening after restart cannot revive a database-scoped case.
	wider := policy.Clone()
	wider.Scope = "cluster"
	c2, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: cfg, ReadPolicy: wider})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.Investigate(ctx, inv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, inv.Scope, inv.ID)
	if err != nil || got.FailureCode != "read_policy_changed" || runner.total() != 0 {
		t.Fatalf("changed policy read: %+v calls=%d err=%v", got, runner.total(), err)
	}
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatal(err)
	}
}

func TestReadPolicyWrapsDeterministicRunbookAndSignalRunner(t *testing.T) {
	base := newScriptedRunner()
	var signalCalls int
	p := &probes.ReadPolicy{Version: 1, Scope: "database", Database: "owned", Binding: "binding"}
	r := policyRunner{base: withSignals(base, []SignalProbe{{ID: probes.SLOStatus, Run: func(_ context.Context, _ probes.Args) probes.Result { signalCalls++; return probes.Result{} }}}), policy: p}
	for _, id := range []probes.ID{probes.ConnectionSaturation, probes.SLOStatus, probes.SignalTarget} {
		if got := r.Run(context.Background(), id, probes.Args{}); got.Reason != probes.ReasonPolicyDenied {
			t.Fatalf("not denied: %+v", got)
		}
	}
	if base.total() != 0 || signalCalls != 0 {
		t.Fatal("denied source was invoked")
	}
}

func TestReadPolicyForgedModelToolCallRemainsDenied(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	base := newScriptedRunner()
	p := &probes.ReadPolicy{Version: 1, Scope: "database", Database: "owned", Binding: "binding"}
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: base, ReadPolicy: p,
		Config: DefaultCoordinatorConfig("policy-forged:" + string(NewUUID()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := c.Start(ctx, lockTrigger("forged-tool"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, c.worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{StateCollecting, StateEvaluating} {
		if _, err := st.CommitStep(ctx, lease, StepResult{IdempotencyKey: string(state), NextState: state}); err != nil {
			t.Fatal(err)
		}
	}
	s := &investigatorSession{c: c, inv: inv, lease: lease}
	tool := s.probeTool()
	if strings.Contains(string(tool.Parameters), string(probes.ConnectionSaturation)) || !strings.Contains(string(tool.Parameters), string(probes.StatDatabase)) {
		t.Fatalf("model tool schema exposes forbidden sources: %s", tool.Parameters)
	}
	// Bypass the model-visible enum deliberately. Execution must enforce
	// the trusted policy even if a provider returns forged arguments.
	out, err := tool.Run(ctx, json.RawMessage(probeArgs(probes.ConnectionSaturation)))
	if err != nil || out.Status != string(probes.StatusUnsupported) || !strings.Contains(out.Text, probes.ReasonPolicyDenied) || base.total() != 0 {
		t.Fatalf("forged read: out=%+v calls=%d err=%v", out, base.total(), err)
	}
	if out.Evidence == nil || out.Evidence.ID == "" {
		t.Fatal("denial is not durable evidence")
	}
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatal(err)
	}
}
