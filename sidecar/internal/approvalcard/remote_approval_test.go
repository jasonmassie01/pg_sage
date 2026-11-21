package approvalcard

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// ux-09a, decision D7: what a person may approve from chat. No concurrent
// access tests: RemoteApprovalAllowed and ActionTier are pure.

func TestRemoteApprovalAllowed_TierByCap(t *testing.T) {
	cases := []struct {
		tier policy.RiskTier
		cap  string
		want bool
	}{
		{policy.RiskSafe, "moderate", true},
		{policy.RiskModerate, "moderate", true},
		{policy.RiskSafe, "safe", true},
		{policy.RiskModerate, "safe", false},
		{policy.RiskSafe, "none", false},
		{policy.RiskModerate, "none", false},
		// No configured cap (a caller without config) is the default.
		{policy.RiskModerate, "", true},
		{policy.RiskSafe, "", true},
	}
	for _, c := range cases {
		ok, reason := RemoteApprovalAllowed(c.tier, c.cap)
		if ok != c.want {
			t.Errorf("tier %s cap %q = %v (%q), want %v", c.tier, c.cap, ok, reason, c.want)
		}
		if ok && reason != "" {
			t.Errorf("tier %s cap %q allowed with reason %q", c.tier, c.cap, reason)
		}
		if !ok && !strings.Contains(reason, string(c.tier)) {
			t.Errorf("tier %s cap %q refusal %q does not name the tier", c.tier, c.cap,
				reason)
		}
	}
}

// CHECK-02: high is never approvable from chat, whatever the key says,
// including values validation would refuse.
func TestRemoteApprovalAllowed_HighIsRefusedWithEveryCap(t *testing.T) {
	for _, cap := range []string{"", "safe", "moderate", "none", "high", "HIGH", "all"} {
		ok, reason := RemoteApprovalAllowed(policy.RiskHigh, cap)
		if ok || !strings.Contains(reason, "high") {
			t.Fatalf("high with cap %q = %v (%q), want refused naming high", cap, ok, reason)
		}
	}
}

// An unknown tier or an unknown cap fails closed.
func TestRemoteApprovalAllowed_UnknownTierOrCapFailsClosed(t *testing.T) {
	for _, tier := range []policy.RiskTier{"", "unknown", "critical", "SAFE"} {
		if ok, reason := RemoteApprovalAllowed(tier, "moderate"); ok || reason == "" {
			t.Fatalf("tier %q allowed (%q)", tier, reason)
		}
	}
	for _, cap := range []string{"high", "all", "Moderate", " safe"} {
		if ok, reason := RemoteApprovalAllowed(policy.RiskSafe, cap); ok ||
			!strings.Contains(reason, cap) {
			t.Fatalf("cap %q allowed safe (%q), want refused naming the cap", cap, reason)
		}
	}
}

func TestActionTier_IsTheHighestKnownTierAndFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		a    store.QueuedAction
		want policy.RiskTier
	}{
		{"safe analyze", store.QueuedAction{ActionType: "analyze_table", ActionRisk: "safe",
			ProposedSQL: "ANALYZE public.t"}, policy.RiskSafe},
		{"stored risk above contract", store.QueuedAction{ActionType: "analyze_table",
			ActionRisk: "high", ProposedSQL: "ANALYZE public.t"}, policy.RiskHigh},
		{"contract above stored risk", store.QueuedAction{ActionType: "terminate_backend",
			ActionRisk: "safe", ProposedSQL: "SELECT pg_terminate_backend(42)"},
			policy.RiskHigh},
		{"moderate index", store.QueuedAction{ActionType: "create_index_concurrently",
			ActionRisk:  "moderate",
			ProposedSQL: "CREATE INDEX CONCURRENTLY i ON public.t (id)"}, policy.RiskModerate},
		{"untyped analyze by its SQL", store.QueuedAction{ActionRisk: "safe",
			ProposedSQL: "ANALYZE public.t"}, policy.RiskSafe},
		{"unknown stored risk", store.QueuedAction{ActionType: "analyze_table",
			ActionRisk: "spicy", ProposedSQL: "ANALYZE public.t"}, policy.RiskHigh},
		{"nothing known", store.QueuedAction{}, policy.RiskHigh},
	}
	for _, c := range cases {
		if got := ActionTier(c.a); got != c.want {
			t.Errorf("%s: tier %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRemoteApprovalRefusal_LinksTheAppAndTheItem(t *testing.T) {
	msg := RemoteApprovalRefusal("step 1 is risk tier high", 42)
	for _, want := range []string{"High-risk changes are approved in pg_sage",
		"step 1 is risk tier high", "#/inbox", "42"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q lacks %q", msg, want)
		}
	}
}

// The card a notifier sends above the cap has no Approve button.
func TestCardRefOfCapped_HidesApproveAboveTheCap(t *testing.T) {
	moderate := Card{Database: "orders", QueueID: 7, Risk: Risk{Tier: "moderate"}}
	if ref := CardRefOfCapped(moderate, "moderate"); ref.NoApprove || ref.ApproveIn != "" {
		t.Fatalf("moderate under the default cap: %+v", ref)
	}
	ref := CardRefOfCapped(moderate, "safe")
	if !ref.NoApprove || !strings.Contains(ref.ApproveIn, "High-risk changes are approved") {
		t.Fatalf("moderate under cap safe: %+v, want no Approve and the app link", ref)
	}
	for _, tier := range []string{"high", "unknown", ""} {
		c := Card{Database: "orders", QueueID: 8, Risk: Risk{Tier: tier}}
		if ref := CardRefOfCapped(c, "moderate"); !ref.NoApprove {
			t.Fatalf("tier %q card offers Approve: %+v", tier, ref)
		}
	}
}
