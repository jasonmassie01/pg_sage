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
		park bool
	}{
		{
			name: "xmin blocker first",
			in: ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed,
				Blocker: &XminBlocker{PID: 42, XminAge: 900, State: "idle in transaction"}},
			kind: ResponseTerminateBlocker, sql: "pg_terminate_backend(42)",
		},
		{
			name: "active xmin blocker is canceled",
			in: ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed,
				Blocker: &XminBlocker{PID: 43, XminAge: 800, State: "active"}},
			kind: ResponseCancelBlocker, sql: "pg_cancel_backend(43)",
		},
		{
			name: "freeze without blocker",
			in:   ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed},
			kind: ResponseVacuumFreeze, sql: `VACUUM (FREEZE) "public"."orders"`,
		},
		{
			name: "safe autovacuum tuning",
			in: ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyAmber,
				DeadTupleRatio: 0.25},
			kind: ResponseTuneAutovacuum, sql: "autovacuum_vacuum_scale_factor = 0.02",
		},
		{
			name: "online bloat plan",
			in: ResponseInput{Schema: "public", Table: "orders", BloatRatio: 0.6,
				PGRepackAvailable: true},
			kind: ResponsePlanRepack, sql: "pg_repack", park: true,
		},
		{
			name: "park bloat without online tool",
			in:   ResponseInput{Schema: "public", Table: "orders", BloatRatio: 0.6},
			kind: ResponseParkBloat, park: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := PlanResponse(test.in)
			if err != nil {
				t.Fatalf("PlanResponse: %v", err)
			}
			if response.Kind != test.kind || response.Park != test.park {
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

func TestPlanResponsePreservesBlockerEvidence(t *testing.T) {
	response, err := PlanResponse(ResponseInput{
		Schema: "public", Table: "orders", Urgency: UrgencyRed,
		Blocker: &XminBlocker{PID: 77, XminAge: 1234, User: "app",
			State: "idle in transaction", Query: "SELECT 1"},
	})
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	if response.Evidence["pid"] != 77 || response.Evidence["xmin_age"] != int64(1234) ||
		response.Evidence["user"] != "app" {
		t.Fatalf("blocker evidence = %#v", response.Evidence)
	}
}

func TestPlanResponseRejectsInvalidEvidence(t *testing.T) {
	for _, input := range []ResponseInput{
		{},
		{Schema: "public", Table: "orders", Blocker: &XminBlocker{PID: -1}},
		{Schema: "public", Table: "orders", DeadTupleRatio: 1.1},
	} {
		if _, err := PlanResponse(input); err == nil {
			t.Fatalf("invalid input accepted: %#v", input)
		}
	}
}
