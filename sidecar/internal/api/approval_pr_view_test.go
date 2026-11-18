package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/gitops"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// cli-02b: what the app shows of approval PRs, and the outcome text and
// tier a PR approval uses. No concurrent access test: the view is one
// mutex-guarded pointer (the gitops DB tests race a merge).

func TestGitopsRoute_CarriesApprovalPRModeOnlyWhenConfigured(t *testing.T) {
	mux := http.NewServeMux()
	registerGitopsRoutes(mux)
	get := func() map[string]any {
		t.Helper()
		rec := requestPolicy(mux, http.MethodGet, "/api/v1/gitops", "", adminFixture.user())
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	SetGitopsReport(nil)
	SetApprovalPRs(nil, nil)
	t.Cleanup(func() { SetApprovalPRs(nil, nil) })
	if got := get(); got["approval_prs"] != nil {
		t.Fatalf("approval PRs shown while off: %v", got)
	}
	SetApprovalPRs(&gitops.ApprovalPRs{}, nil)
	if got := get(); got["approval_prs"] != nil {
		t.Fatalf("a runner without a repo shown as configured: %v", got)
	}
	SetApprovalPRs(&gitops.ApprovalPRs{Config: gitops.ApprovalPRConfig{Repo: "acme/cfg"}},
		nil)
	got, _ := get()["approval_prs"].(map[string]any)
	if got["enabled"] != true || got["repo"] != "acme/cfg" || got["disabled_reason"] != nil {
		t.Fatalf("approval_prs = %v, want enabled on acme/cfg", got)
	}
}

func TestApprovalPRNote_AppOnlyAboveTheCapAndNothingWhenOff(t *testing.T) {
	a := &store.QueuedAction{ID: 4, Status: "pending", ActionRisk: "high",
		ProposedSQL: "VACUUM FULL public.t"}
	c := approvalcard.Card{QueueID: 4, Risk: approvalcard.Risk{Tier: "high"}}
	SetApprovalPRs(nil, nil)
	t.Cleanup(func() { SetApprovalPRs(nil, nil) })
	if n := approvalPRNote(context.Background(), "orders", c, a); n != nil {
		t.Fatalf("note while off = %v", n)
	}
	// With PR mode on, LookupApprovalPR needs the control database; a nil
	// pool is an error, which the inbox treats as no note (fail quiet).
	SetApprovalPRs(&gitops.ApprovalPRs{Config: gitops.ApprovalPRConfig{Repo: "o/r"}}, nil)
	if n := approvalPRNote(context.Background(), "orders", c, a); n != nil {
		t.Fatalf("note without a control database = %v", n)
	}
}

func TestApprovalOutcomeText(t *testing.T) {
	cases := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"executed": true, "verification_status": "monitoring"},
			"ran on orders (verification: monitoring)"},
		{map[string]any{"status": "scheduled", "run_after_label": "Sat 02:00"},
			"scheduled for the maintenance window opening Sat 02:00"},
		{map[string]any{"executed": false, "error": "lock timeout"},
			"approved, but the change did not run: lock timeout"},
	}
	for _, tc := range cases {
		if got := approvalOutcomeText("orders", tc.body); got != tc.want {
			t.Errorf("approvalOutcomeText(%v) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestCappedTier_TakesTheHigherAndFailsClosed(t *testing.T) {
	a := store.QueuedAction{ActionRisk: "safe", ProposedSQL: "ANALYZE public.t",
		ActionType: "analyze_table"}
	base := approvalcard.ActionTier(a)
	if got := cappedTier(a, approvalcard.Card{}); got != base {
		t.Fatalf("no card tier: %s, want the action's %s", got, base)
	}
	if got := cappedTier(a, approvalcard.Card{Risk: approvalcard.Risk{Tier: "high"}}); got !=
		policy.RiskHigh {
		t.Fatalf("card tier high: %s, want high", got)
	}
	if got := cappedTier(a, approvalcard.Card{Risk: approvalcard.Risk{Tier: "weird"}}); got !=
		policy.RiskHigh {
		t.Fatalf("unknown card tier: %s, want high (fail closed)", got)
	}
}
