package rca

import (
	"encoding/json"
	"testing"
	"time"
)

// The persisted causal_chain must keep the structured blocker identity
// and stay valid JSON for any text (fmt %q emits \x escapes that jsonb
// rejects).
func TestMarshalChain_BlockerRoundTripAndValidJSON(t *testing.T) {
	start := time.Date(2026, 9, 27, 8, 0, 0, 123456000, time.UTC)
	chain := []ChainLink{
		{Order: 1, Signal: "lock_contention", Description: "ctl\x01char",
			Evidence: "bad utf8 \xff"},
		{Order: 2, Signal: "lock_contention", Description: "blocker",
			Blocker: &BlockerIdentity{PID: 42, BackendStart: start,
				QueryID: -7, State: "idle in transaction"}},
	}
	raw, err := marshalChain(chain)
	if err != nil {
		t.Fatalf("marshalChain: %v", err)
	}
	if !json.Valid([]byte(raw)) {
		t.Fatalf("marshalChain produced invalid JSON: %s", raw)
	}
	var back []ChainLink
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 2 || back[0].Blocker != nil || back[1].Blocker == nil {
		t.Fatalf("round trip = %+v", back)
	}
	b := back[1].Blocker
	if b.PID != 42 || !b.BackendStart.Equal(start) || b.QueryID != -7 ||
		b.State != "idle in transaction" {
		t.Fatalf("blocker round trip = %+v", b)
	}
}

func TestMarshalChain_UnencodableTimeIsAnError(t *testing.T) {
	bad := []ChainLink{{Order: 1, Blocker: &BlockerIdentity{
		PID: 1, BackendStart: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}}
	if got, err := marshalChain(bad); err == nil {
		t.Fatalf("year-10000 timestamp encoded as %s, want error", got)
	}
}
