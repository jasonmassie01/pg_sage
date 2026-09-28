package sre

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Evidence keeps 64-bit identities exact: a queryid above 2^53 must not
// be rounded by the canonical form the hash is computed over, or a plan
// investigation would diagnose a different query than it was asked to.
func TestEvidence_LargeIntegersSurviveStorageExactly(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	inv, _, _ := st.Create(ctx, lockStart(scope, "queryid big"))
	lease, err := st.Claim(ctx, scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	const qid = int64(1234567890123456789)
	res := rows(probes.PlanRegressions, probes.Row{"queryid": qid, "after_mean_ms": 2.5,
		"before_calls": int64(10), "after_calls": int64(10), "before_mean_ms": 1.0,
		"plan_flipped": false, "current_plan_hash": "v1:a"})
	if _, err := st.CommitStep(ctx, lease, step("s", StateCollecting, res)); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev, err := st.Evidence(ctx, scope, inv.ID)
	if err != nil || len(ev) != 1 {
		t.Fatalf("evidence = %+v (%v)", ev, err)
	}
	if !strings.Contains(string(ev[0].Payload), "1234567890123456789") {
		t.Fatalf("payload lost the exact queryid: %s", ev[0].Payload)
	}
	if !ev[0].VerifyHash() {
		t.Fatal("hash does not verify after the jsonb round trip")
	}
	obs, err := observations(ev)
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	shifts, err := probes.PlanShifts(obs[0].Result)
	if err != nil || len(shifts) != 1 || shifts[0].QueryID != qid {
		t.Fatalf("decoded shifts = %+v (%v), want queryid %d", shifts, err, qid)
	}
}
