package tuner

import "testing"

// PG18 reports "Actual Rows" with two decimals. Parsing it as an integer
// failed ScanPlan, so every symptom in an ANALYZE plan was missed.
func TestScanPlanAcceptsPG18FractionalActualRows(t *testing.T) {
	plan := []byte(`[{"Plan":{"Node Type":"Nested Loop","Plan Rows":2,
		"Actual Rows":50.50,"Actual Loops":1,"Plans":[
		{"Node Type":"Seq Scan","Relation Name":"orders","Plan Rows":2,
		 "Actual Rows":0.50,"Actual Loops":101}]}}]`)
	symptoms, err := ScanPlan(plan)
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	var nested *PlanSymptom
	for i := range symptoms {
		if symptoms[i].Kind == SymptomBadNestedLoop {
			nested = &symptoms[i]
		}
	}
	if nested == nil {
		t.Fatalf("bad nested loop missed: %+v", symptoms)
	}
	// The detail keeps its int64 contract; PG18's per-loop average rounds.
	if got := nested.Detail["actual_rows"]; got != int64(51) {
		t.Fatalf("actual_rows detail = %v (%T), want int64 51", got, got)
	}
	// Just under the 10x threshold must not fire.
	quiet := []byte(`[{"Plan":{"Node Type":"Nested Loop","Plan Rows":2,"Actual Rows":20.00}}]`)
	symptoms, err = ScanPlan(quiet)
	if err != nil {
		t.Fatalf("ScanPlan: %v", err)
	}
	for _, s := range symptoms {
		if s.Kind == SymptomBadNestedLoop {
			t.Fatalf("10x exactly flagged as bad nested loop: %+v", s)
		}
	}
}
