package approvalcard

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// indexes-06: an approval card for a partially measured index names the
// statements the what-if could not plan (CHECK-04).
func TestAssemblePartialWhatIfShowsUnplannedQueryIDs(t *testing.T) {
	a := queued(4, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON public.t (a)", "DROP INDEX CONCURRENTLY public.i",
		"moderate")
	reason := "planned statements improved 62.0% (call-weighted, 4 of 5); " +
		"not planned: queryid 123"
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "Index", Detail: map[string]any{
			"what_if_verdict": "unverified", "what_if_reason": reason,
			"what_if_measured": float64(4), "what_if_unplanned_queryids": []any{float64(123)},
			"estimated_improvement_pct": 62.0}}})
	p := c.Predicted
	if p.WhatIfMeasured == nil || *p.WhatIfMeasured != 4 ||
		!reflect.DeepEqual(p.WhatIfUnplannedQueryIDs, []int64{123}) ||
		p.WhatIfReason != reason || !hasCode(c, "what_if_unverified") {
		t.Fatalf("predicted = %+v why %v", p, reasonCodes(c))
	}
	if ev := evidenceLabels(c); strings.Contains(ev, "what if") {
		t.Fatalf("the predicted coverage is repeated as evidence:\n%s", ev)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	for _, want := range []string{`"what_if_unplanned_queryids":[123]`,
		`"what_if_measured":4`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("card JSON lacks %s:\n%s", want, raw)
		}
	}
}

func TestAssembleCompleteWhatIfOmitsUnplanned(t *testing.T) {
	a := queued(5, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON public.t (a)", "DROP INDEX CONCURRENTLY public.i",
		"moderate")
	c := Assemble(Inputs{Database: "db", Action: a, Now: now, Contract: contractOf(t, a),
		Finding: &FindingRow{ID: a.FindingID, Title: "Index", Detail: map[string]any{
			"what_if_verdict": "verified", "what_if_unplanned_queryids": "not a list"}}})
	if c.Predicted.WhatIfMeasured != nil || c.Predicted.WhatIfUnplannedQueryIDs != nil {
		t.Fatalf("predicted = %+v: nothing measured partially", c.Predicted)
	}
}
