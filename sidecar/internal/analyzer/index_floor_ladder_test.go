package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-04a: a floor finding carries its candidate's ladder records in
// detail["ladder"], where the executor's findingLadder reads them.
func TestFloorFindings_CarryTheLadder(t *testing.T) {
	pct := -40.0
	rec := verify.FromRehearsal(verify.Rehearsal{Passed: true, BlocksPerCallPct: &pct,
		ReceiptID: 3, Provider: "lakebase_branch"}, time.Now())
	adm := optimizer.Admission{Outcome: optimizer.AdmitAccepted,
		Rec: optimizer.Recommendation{Table: "public.orders", WhatIf: optimizer.WhatIfVerified,
			DDL:    "CREATE INDEX CONCURRENTLY o_c ON public.orders (customer_id)",
			Ladder: []verify.LadderRecord{rec}}}
	plain := adm
	plain.Rec.Ladder = nil
	plain.Rec.DDL = "CREATE INDEX CONCURRENTLY o_r ON public.orders (region)"
	out := floorFindings([]optimizer.Admission{adm, plain}, nil)
	if len(out) != 2 {
		t.Fatalf("findings %+v", out)
	}
	recs, err := verify.DecodeLadderValue(out[0].Detail["ladder"])
	if err != nil || len(recs) != 1 || !recs[0].Passed() || recs[0].Tier != verify.TierClone ||
		recs[0].Evidence["receipt_id"] != float64(3) {
		t.Fatalf("ladder %+v (%v)", recs, err)
	}
	if _, ok := out[1].Detail["ladder"]; ok {
		t.Fatalf("no records, no ladder key: %+v", out[1].Detail)
	}
}
