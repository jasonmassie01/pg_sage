package inbox

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Pure functions: no DB, no concurrency.

func TestParseKinds(t *testing.T) {
	all, err := ParseKinds(" ")
	if err != nil || len(all) != 7 {
		t.Fatalf("empty: %v %v, want every kind", all, err)
	}
	got, err := ParseKinds("fact, change_approval,fact")
	if err != nil || len(got) != 2 || got[0] != KindChangeApproval || got[1] != KindFact {
		t.Fatalf("got %v %v, want change_approval,fact in fixed order", got, err)
	}
	for _, bad := range []string{"facts", "fact,", "FACT", "change_approval;drop"} {
		if _, err := ParseKinds(bad); err == nil ||
			!strings.Contains(err.Error(), "known kinds: change_approval") {
			t.Fatalf("ParseKinds(%q) = %v, want an error naming the known kinds", bad, err)
		}
	}
	if got, err := KindsOf(nil); err != nil || len(got) != 7 {
		t.Fatalf("KindsOf(nil) = %v %v", got, err)
	}
}

func TestNormalizeRisk(t *testing.T) {
	for in, want := range map[string]string{"HIGH_RISK": RiskHigh, "high": RiskHigh,
		"critical": RiskHigh, "SAFE": RiskSafe, "low": RiskSafe, "": RiskModerate,
		"moderate": RiskModerate, "weird": RiskModerate} {
		if got := NormalizeRisk(in); got != want {
			t.Errorf("NormalizeRisk(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanDecide(t *testing.T) {
	cases := []struct {
		kind Kind
		role string
		want bool
	}{
		{KindChangeApproval, "operator", true}, {KindFact, "operator", true},
		{KindAutonomyPromotion, "operator", false}, {KindRunbookSignature, "operator", false},
		{KindPolicyException, "operator", false}, {KindPolicyException, "admin", true},
		{KindChangeApproval, "viewer", false}, {KindChangeApproval, "agent", false},
		{KindChangeApproval, "", false},
	}
	for _, c := range cases {
		if got := CanDecide(c.kind, c.role); got != c.want {
			t.Errorf("CanDecide(%s, %q) = %v, want %v", c.kind, c.role, got, c.want)
		}
	}
}

func TestHrefEscapesEveryValue(t *testing.T) {
	got := href("actions", "tab", "pending", "queue", "7", "database", "a&b=c#d")
	if got != "#/actions?tab=pending&queue=7&database=a%26b%3Dc%23d" {
		t.Fatalf("href = %s", got)
	}
	if href("trust") != "#/trust" {
		t.Fatalf("no params: %s", href("trust"))
	}
}

func TestPlanItemWords(t *testing.T) {
	since := time.Now()
	halted := planItem("orders", changeplan.NeedingPerson{PlanID: 4, Goal: "g",
		Status: changeplan.StatusHalted, Since: since})
	if halted.ID != "4" || halted.Risk != RiskHigh || !strings.Contains(halted.Why, "halted") ||
		halted.Href != "#/actions?tab=plans&plan=4&database=orders" {
		t.Fatalf("halted %+v", halted)
	}
	step := planItem("orders", changeplan.NeedingPerson{PlanID: 4, Goal: "g",
		Status: changeplan.StatusRunning, StepOrdinal: 2,
		StepState: "awaiting_operator", Reason: "run it by hand"})
	if step.ID != "4/2" || step.Source != "sage.plan_step" ||
		step.Why != "step 2 is awaiting_operator: run it by hand" {
		t.Fatalf("step %+v", step)
	}
}

func TestPolicyItemWidening(t *testing.T) {
	p := policy.Proposal{ID: 9, Profile: "staffed", Actor: "mcp:token:1",
		PrincipalID: "agp_abcdefghijklmnopqrst", Widening: true}
	it := policyItem("orders", p)
	if it.Risk != RiskHigh || !strings.Contains(it.Title, "agent agp_") ||
		it.Href != "#/agents?tab=policy&proposal=9&database=orders" {
		t.Fatalf("widening %+v", it)
	}
	p.Widening, p.PrincipalID = false, ""
	if it := policyItem("orders", p); it.Risk != RiskModerate ||
		!strings.HasSuffix(it.Title, "by mcp:token:1") {
		t.Fatalf("narrowing %+v", it)
	}
}
