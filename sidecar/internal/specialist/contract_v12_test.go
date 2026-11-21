package specialist

import (
	"context"
	"encoding/json"
	"testing"
)

// Contract revision 1.2.0 (mcp-02), additive within v1: results carry
// summary_for_agent, an ignored adapter event may carry a code, and the
// incident.io and Azure Monitor adapters are published.

func v12Result(t *testing.T) []byte {
	t.Helper()
	h := remediationHarness(t)
	r, err := h.svc.Result(context.Background(), proposer, "orders", string(inv))
	if err != nil {
		t.Fatal(err)
	}
	if r.SummaryForAgent == nil {
		t.Fatal("fixture must carry summary_for_agent")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCompat_V12ResultValidatesAgainstBaselineAndCurrent(t *testing.T) {
	value := decodeNumbers(t, v12Result(t))
	for name, doc := range map[string]map[string]any{"baseline": baselineDoc(t),
		"current": currentDoc(t)} {
		if p := validateValue(doc, schemaNamed(doc, "Result"), value, "$"); len(p) > 0 {
			t.Errorf("against %s: %v", name, p)
		}
	}
}

func TestCompat_V12SummaryIsValidatedNotJustTolerated(t *testing.T) {
	doc := currentDoc(t)
	for name, mutate := range map[string]func(s map[string]any){
		"missing status":  func(s map[string]any) { delete(s, "status") },
		"evidence object": func(s map[string]any) { s["evidence"] = map[string]any{} },
		"bad action":      func(s map[string]any) { s["actions"] = []any{"x"} },
		"why-not string":  func(s map[string]any) { s["why_no_action"] = "no" },
	} {
		value := decodeNumbers(t, v12Result(t)).(map[string]any)
		mutate(value["summary_for_agent"].(map[string]any))
		if p := validateValue(doc, schemaNamed(doc, "Result"), value, "$"); len(p) == 0 {
			t.Errorf("%s was not detected", name)
		}
	}
}

func TestCompat_V12FieldsAreOptional(t *testing.T) {
	doc := currentDoc(t)
	for schema, field := range map[string]string{"Result": "summary_for_agent",
		"WebhookIgnored": "code"} {
		s := schemaNamed(doc, schema)
		if _, ok := s["properties"].(map[string]any)[field]; !ok {
			t.Errorf("%s.%s is not documented", schema, field)
		}
		if stringSet(s["required"])[field] {
			t.Errorf("%s.%s must be optional in v1", schema, field)
		}
	}
}

func TestCompat_V12AdapterPathsArePublished(t *testing.T) {
	paths := currentDoc(t)["paths"].(map[string]any)
	for path, opID := range map[string]string{"/adapters/incidentio": "incidentIOWebhook",
		"/adapters/azure-monitor": "azureMonitorAlert"} {
		op, ok := paths[path].(map[string]any)
		if !ok {
			t.Errorf("%s is not published", path)
			continue
		}
		post := op["post"].(map[string]any)
		if post["operationId"] != opID {
			t.Errorf("%s operationId %v", path, post["operationId"])
		}
		responses := post["responses"].(map[string]any)
		for _, code := range []string{"200", "201", "202", "400", "401", "413"} {
			if _, ok := responses[code]; !ok {
				t.Errorf("%s response %s missing", path, code)
			}
		}
	}
}
