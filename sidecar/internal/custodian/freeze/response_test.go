package freeze

import (
	"strings"
	"testing"
)

func TestPlanResponseUsesSafeOrderedLadder(t *testing.T) {
	tests := []struct {
		name string
		in   ResponseInput
		kind ResponseKind
		sql  string
	}{
		{
			// vacuum-02: a 30-XID snapshot cannot pin a table at 160M, so the
			// table is frozen (before, the oldest session was canceled instead).
			name: "young session holder still freezes",
			in: ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed,
				XIDAge: 160_000_000,
				Holder: &XminBlocker{Kind: HolderSession, PID: 42, XminAge: 30,
					State: "idle in transaction"}},
			kind: ResponseVacuumFreeze, sql: `VACUUM (FREEZE) "public"."orders"`,
		},
		{
			name: "freeze without blocker",
			in:   ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed},
			kind: ResponseVacuumFreeze, sql: `VACUUM (FREEZE) "public"."orders"`,
		},
		{
			name: "online bloat plan",
			in: ResponseInput{Schema: "public", Table: "orders", BloatRatio: 0.6,
				BloatKnown: true, SizeBytes: 2 << 30, MinBloatBytes: 1 << 30,
				PGRepackAvailable: true},
			kind: ResponsePlanRepack, sql: "pg_repack",
		},
		{
			name: "park bloat without online tool",
			in: ResponseInput{Schema: "public", Table: "orders", BloatRatio: 0.6,
				BloatKnown: true, SizeBytes: 2 << 30, MinBloatBytes: 1 << 30},
			kind: ResponseParkBloat,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := PlanResponse(test.in)
			if err != nil {
				t.Fatalf("PlanResponse: %v", err)
			}
			if response.Kind != test.kind {
				t.Fatalf("response = %#v", response)
			}
			text := response.SQL + response.Plan
			if test.sql != "" && !strings.Contains(text, test.sql) {
				t.Fatalf("response text = %q, want %q", text, test.sql)
			}
			if strings.Contains(strings.ToUpper(text), "VACUUM FULL") {
				t.Fatalf("unsafe VACUUM FULL response = %#v", response)
			}
		})
	}
}

func TestPlanResponseRejectsInvalidEvidence(t *testing.T) {
	for _, input := range []ResponseInput{
		{},
		{Schema: "public", Table: "orders", XIDAge: 10,
			Holder: &XminBlocker{Kind: HolderSession, PID: -1}},
		{Schema: "public", Table: "orders", XIDAge: -1},
		{Schema: "public", Table: "orders", XIDAge: 10, Holder: &XminBlocker{XminAge: 5}},
		{Schema: "public", Table: "orders", XIDAge: 10,
			Holder: &XminBlocker{Kind: HolderSlot, Name: "s", XminAge: -1}},
		{Schema: "public", Table: "orders", DeadTupleRatio: 1.1},
	} {
		if _, err := PlanResponse(input); err == nil {
			t.Fatalf("invalid input accepted: %#v", input)
		}
	}
}
