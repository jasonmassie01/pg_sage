package outcomes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-03a: the ledger row shows the highest verification-ladder tier the
// change passed before production, from the prediction stored with it.

func predictedWithLadder(t *testing.T, recs ...verify.LadderRecord) []byte {
	t.Helper()
	pct := -40.0
	raw, err := json.Marshal(verify.Prediction{Class: "index_create",
		Method: verify.MethodHypoPG, Metric: "blocks_per_call", ExpectedChangePct: &pct,
		Ladder: recs})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTierOf_HighestPassedTier(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	tier1 := verify.FromWhatIf(verify.WhatIfEvidence{Verdict: "verified"}, at)
	clone := verify.FromRehearsal(verify.Rehearsal{Passed: true, ReceiptID: 812}, at)
	failedClone := verify.FromRehearsal(verify.Rehearsal{Reason: "regressed"}, at)
	cases := []struct {
		name      string
		predicted []byte
		want      string
	}{
		{"no prediction", nil, ""},
		{"prediction without a ladder", predictedWithLadder(t), ""},
		{"tier 1", predictedWithLadder(t, tier1, failedClone), "plan_only"},
		{"tier 2", predictedWithLadder(t, tier1, clone), "clone"},
		{"unreadable", []byte(`{"ladder":"x"}`), ""},
		{"not json", []byte(`nope`), ""},
	}
	for _, c := range cases {
		if got := tierOf(c.predicted); got != c.want {
			t.Errorf("%s: tierOf = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuild_FillsTheTier(t *testing.T) {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	s := scanned{outcome: "success", predicted: predictedWithLadder(t,
		verify.FromRehearsal(verify.Rehearsal{Passed: true}, at))}
	r := build(Row{ActionID: 1, ActionType: "create_index"}, s, "orders")
	if r.Tier != "clone" {
		t.Fatalf("row tier = %q, want clone", r.Tier)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["tier"] != "clone" {
		t.Fatalf("row JSON tier = %v (%v)", m["tier"], err)
	}
}
