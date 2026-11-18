package api

import (
	"net/http"
	"strings"
	"testing"
)

// evid-03a: POST /api/v1/facts declares an owner brief unchanged (type
// owner_brief, subject kind database, subject "*"); a second declaration
// replaces it in place; invalid briefs are refused as invalid facts.
//
// No concurrent access tests here: the store's concurrent declarations
// are covered in internal/facts (owner_brief_db_test.go).

func briefBody(value map[string]string) map[string]any {
	return map[string]any{"type": "owner_brief", "subject_kind": "database",
		"subject": "*", "value": value, "note": "from the owner"}
}

func TestFactRoutesOwnerBrief_DeclareAndList(t *testing.T) {
	f := newFactAPI(t)
	code, body := f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb",
		briefBody(map[string]string{"owner": "Payments",
			"peak_windows": "weekdays 09:00-17:00", "growth_pct_month": "5"}))
	fact, _ := body["fact"].(map[string]any)
	if code != http.StatusCreated || fact["status"] != "confirmed" ||
		fact["subject_kind"] != "database" || fact["subject"] != "*" {
		t.Fatalf("declare brief: %d %v", code, body)
	}
	summary, _ := fact["summary"].(string)
	if !strings.HasPrefix(summary, "owner brief: owner Payments, peak weekdays") {
		t.Fatalf("summary = %q", summary)
	}
	code, body = f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb",
		briefBody(map[string]string{"owner": "Ledger"}))
	again, _ := body["fact"].(map[string]any)
	if code != http.StatusCreated || again["id"] != fact["id"] ||
		again["proposals"] != float64(2) {
		t.Fatalf("re-declare: %d %v (first %v)", code, body, fact["id"])
	}
	code, body = f.do(t, viewerUser(), "GET",
		"/api/v1/facts?database=testdb&type=owner_brief", nil)
	list := asList(body["facts"])
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("list briefs: %d %v", code, body)
	}
	listed, _ := list[0].(map[string]any)
	value, _ := listed["value"].(map[string]any)
	if value["owner"] != "Ledger" || len(value) != 1 {
		t.Fatalf("listed brief value = %v, want the replacement", value)
	}
}

func TestFactRoutesOwnerBrief_InvalidRefused(t *testing.T) {
	f := newFactAPI(t)
	for name, b := range map[string]map[string]any{
		"bad window": briefBody(map[string]string{"peak_windows": "lunch"}),
		"negative growth": briefBody(map[string]string{
			"growth_pct_month": "-1"}),
		"no value":    briefBody(nil),
		"unknown key": briefBody(map[string]string{"tables": "orders"}),
		"subject other": {"type": "owner_brief", "subject_kind": "database",
			"subject": "public", "value": map[string]string{"owner": "a"}},
		"kind table": {"type": "owner_brief", "subject_kind": "table",
			"subject": "public.orders", "value": map[string]string{"owner": "a"}},
	} {
		code, body := f.do(t, testOperatorUser(), "POST", "/api/v1/facts?database=testdb", b)
		if code != http.StatusBadRequest || body["code"] != "invalid_fact" {
			t.Errorf("%s: %d %v, want 400 invalid_fact", name, code, body)
		}
	}
	if code, body := f.do(t, viewerUser(), "POST", "/api/v1/facts?database=testdb",
		briefBody(map[string]string{"owner": "a"})); code != http.StatusForbidden {
		t.Fatalf("a viewer declared a brief: %d %v", code, body)
	}
}
