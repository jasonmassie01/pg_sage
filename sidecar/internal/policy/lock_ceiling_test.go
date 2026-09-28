package policy

import (
	"context"
	"testing"
)

// D1: lock_duration_ceiling_ms caps lock_timeout for in-transaction DDL.
// lock_timeout = 0 disables the timeout in PostgreSQL, so a zero on either
// side means "no bound from this source", never "wait zero".
func TestEffectiveLockTimeoutMS(t *testing.T) {
	tests := []struct {
		name            string
		ceiling, safety int64
		want            int64
	}{
		{"ceiling tighter than safety", 3000, 30000, 3000},
		{"ceiling equal to safety", 3000, 3000, 3000},
		{"ceiling equal to large safety", 30000, 30000, 30000},
		{"safety tighter than ceiling", 30000, 3000, 3000},
		{"one millisecond apart", 2999, 3000, 2999},
		{"no ceiling keeps safety", 0, 30000, 30000},
		{"negative ceiling keeps safety", -1, 30000, 30000},
		{"safety disabled uses ceiling", 3000, 0, 3000},
		{"both unset", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveLockTimeoutMS(tt.ceiling, tt.safety); got != tt.want {
				t.Fatalf("EffectiveLockTimeoutMS(%d, %d) = %d, want %d",
					tt.ceiling, tt.safety, got, tt.want)
			}
		})
	}
}

func TestExecuteDecisionCarriesPolicyLockCeiling(t *testing.T) {
	doc := refusalDoc()
	doc.LockDurationCeilingMS = 2500
	req := refusalRequest(moderateContract(RollbackReversible, ""),
		"ALTER TABLE public.orders SET (fillfactor = 90)")

	self := authorize(t, doc, req)
	req.OperatorApproved = true
	operator := authorize(t, doc, req)

	for name, got := range map[string]Decision{"self": self, "operator": operator} {
		if got.Verdict != VerdictExecute || got.LockCeilingMS != 2500 {
			t.Fatalf("%s decision = %s ceiling=%d, want execute ceiling=2500",
				name, got.Verdict, got.LockCeilingMS)
		}
	}
}

func TestNonExecuteDecisionCarriesNoLockCeiling(t *testing.T) {
	doc := refusalDoc()
	advisory := refusalRuntime()
	advisory.TrustLevel = TrustAdvisory
	req := refusalRequest(moderateContract(RollbackReversible, ""),
		"ALTER TABLE public.orders SET (fillfactor = 90)")

	got := refusalGate(doc, advisory).Authorize(context.Background(), req)

	if got.Verdict != VerdictQueueApproval || got.LockCeilingMS != 0 {
		t.Fatalf("decision = %s ceiling=%d, want queue_approval ceiling=0",
			got.Verdict, got.LockCeilingMS)
	}
}
