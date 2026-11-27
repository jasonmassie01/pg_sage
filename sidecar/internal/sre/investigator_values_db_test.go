package sre

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-03: a read the investigator collects is committed as evidence and
// handed to the agent loop with its typed values, so the claims citing it
// are grounded in those values and not in the formatted text.
// No concurrency test: one session collects one read at a time.

func TestCollect_EvidenceCarriesTheReadsTypedValues(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: newScriptedRunner(),
		Config: DefaultCoordinatorConfig("values:" + string(NewUUID()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := c.Start(ctx, lockTrigger("typed-values"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, c.worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{StateCollecting, StateEvaluating} {
		if _, err := st.CommitStep(ctx, lease, StepResult{IdempotencyKey: string(state),
			NextState: state}); err != nil {
			t.Fatal(err)
		}
	}
	s := &investigatorSession{c: c, inv: inv, lease: lease}
	res := rows(probes.LongTransactions,
		probes.Row{"pid": int64(4242), "xact_age_s": 75.3125},
		probes.Row{"pid": int64(-1), "xact_age_s": 3.0})
	out, err := s.collect(ctx, func(context.Context) probes.Result { return res })
	if err != nil || out.Evidence == nil || out.Evidence.ID == "" {
		t.Fatalf("collect = %+v, err %v", out, err)
	}
	got := strings.Join(out.Evidence.Values, "|")
	if got != "4242|75.31|-1|3" {
		t.Fatalf("evidence values = %q, want the shown row values", got)
	}
	if !strings.Contains(out.Evidence.Text, "2 rows") {
		t.Fatalf("evidence text = %q, want the formatted result", out.Evidence.Text)
	}
	cited := []agentloop.Evidence{*out.Evidence}
	if err := groundClaim("pid -1 has been open 3 s.", cited); err != nil {
		t.Fatalf("a shown value was not grounded: %v", err)
	}
	if err := groundClaim("2 transactions are open.", cited); err == nil {
		t.Fatal("the row count grounded a claim")
	}
}
