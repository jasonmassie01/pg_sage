package cases

import (
	"encoding/json"
	"strings"
	"testing"
)

// tuning-10: a case's evidence carries the exact pg_stat_statements id as
// a decimal string, so the dashboard's Explain panel never sends an id a
// browser rounded (JavaScript numbers are exact only to 2^53).

func TestEvidenceForFinding_CarriesExactQueryID(t *testing.T) {
	f := SourceFinding{ObjectIdentifier: "queryid:-5191732810777595558",
		Detail: map[string]any{"queryid": float64(-5191732810777595558)}}
	ev := evidenceForFinding(f)
	if len(ev) != 1 || ev[0].QueryID != "-5191732810777595558" {
		t.Fatalf("evidence = %+v, want queryid -5191732810777595558", ev)
	}
	raw, err := json.Marshal(ev[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"queryid":"-5191732810777595558"`) {
		t.Errorf("JSON = %s, want the id as a string", raw)
	}
}

// A conflicting or missing id is left out rather than guessed.
func TestEvidenceForFinding_NoQueryIDWhenUnverified(t *testing.T) {
	for _, f := range []SourceFinding{
		{ObjectIdentifier: "queryid:42", Detail: map[string]any{"queryid": "41"}},
		{ObjectIdentifier: "public.orders", Detail: map[string]any{"table": "orders"}},
		{},
	} {
		ev := evidenceForFinding(f)
		if ev[0].QueryID != "" {
			t.Errorf("%+v: QueryID = %q, want none", f, ev[0].QueryID)
		}
		raw, _ := json.Marshal(ev[0])
		if strings.Contains(string(raw), `"queryid":`) && f.Detail["queryid"] == nil {
			t.Errorf("JSON = %s, want no queryid key", raw)
		}
	}
}

func TestQueryHintEvidence_CarriesExactQueryID(t *testing.T) {
	ev := queryHintEvidence(SourceQueryHint{QueryID: 9223372036854775807, Status: "active"})
	if len(ev) == 0 || ev[0].QueryID != "9223372036854775807" {
		t.Fatalf("evidence = %+v, want the exact id string", ev)
	}
}
