package inbox

import (
	"context"
	"strings"
	"testing"
)

// ux-07a: a promotion the approval history earned asks in the person's
// words, with the count and the database; any other promotion keeps the
// ledger's title.

func TestInboxDB_ApprovalHistoryPromotionAsksInYourWords(t *testing.T) {
	pool := inboxDB(t, "inbox_ask")
	svc, _ := inboxFleet(t, pool, "orders")
	mustExec(t, pool, `INSERT INTO sage.sre_autonomy_proposals (deployment_id,
		database_name, id, family, action_class, from_level, to_level, evidence,
		evidence_sha256, status, expires_at) VALUES ($1, 'orders', gen_random_uuid(),
		'hygiene', 'analyze', 2, 3, '{"kind": "approval_history", "evidence":
		{"approval_history": {"approvals": 14, "required": 10, "qualifies": true}}}',
		sha256('a'::bytea), 'pending', clock_timestamp() + interval '1 hour')`,
		testDeployment)
	res := svc.List(context.Background(), Request{Databases: []string{"orders"},
		Kinds: []Kind{KindAutonomyPromotion}})
	it, ok := byKind(res)[KindAutonomyPromotion]
	want := "You approved analyze 14 times on orders with no rollback; allow it unattended?"
	if !ok || it.Title != want || !strings.Contains(it.Why, "Not now") ||
		it.Risk != RiskHigh {
		t.Fatalf("item = %+v (errors %v), want the ask %q", it, res.Errors, want)
	}
}
