package firstlook

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/agentposture"
)

func TestPostureItem_CarriesTheFindingAsAManualScript(t *testing.T) {
	f := agentposture.Finding{Detector: "AP-03", Severity: agentposture.Critical,
		ObjectType: "table", Object: "public.orders", Title: "orders is readable by anon",
		Detail: "RLS is disabled", Recommendation: "Enable RLS with policies, or revoke",
		FixScript: "ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY;",
		Caveat:    "Without a policy, enabling RLS denies every row",
		Evidence: []agentposture.Evidence{{Source: "pg_class.relacl", Ref: "public.orders",
			Detail: "anon=r"}}}
	it := postureItem(f)
	if it.Rule != "AP-03" || it.Section != SectionAgentPosture ||
		it.Severity != SeverityCritical || it.Object != "public.orders" ||
		it.Title != f.Title || it.Detail != f.Detail ||
		it.Recommendation != f.Recommendation || it.SuggestedSQL != f.FixScript ||
		it.Caveat != f.Caveat {
		t.Fatalf("item = %+v", it)
	}
	if len(it.Evidence) != 1 || it.Evidence[0].Source != "pg_class.relacl" ||
		it.Evidence[0].Ref != "public.orders" || it.Evidence[0].Detail != "anon=r" {
		t.Fatalf("evidence = %+v", it.Evidence)
	}
}

func TestPostureItem_NilEvidenceBecomesEmpty(t *testing.T) {
	it := postureItem(agentposture.Finding{Detector: "AP-07", Severity: agentposture.Warning,
		ObjectType: "schema", Object: "public", Title: "PUBLIC can create"})
	if it.Evidence == nil || len(it.Evidence) != 0 {
		t.Fatalf("evidence = %#v, want an empty list", it.Evidence)
	}
}

func TestSortItems_PostureFollowsSeverityOrder(t *testing.T) {
	items := []Item{
		{Rule: RuleDuplicateIndex, Severity: SeverityWarning, Object: "a"},
		{Rule: "AP-03", Section: SectionAgentPosture, Severity: SeverityCritical, Object: "b"},
	}
	sortItems(items)
	if items[0].Rule != "AP-03" {
		t.Fatalf("order = %+v, want the critical posture item first", items)
	}
}
