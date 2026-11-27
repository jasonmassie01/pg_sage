package tuning

import (
	"testing"
	"time"
)

// No concurrent access tests: the explain tool reads one Store answer per
// call and keeps no state of its own.

func TestTools_ExplainCarriesCaptureTimeAndAge(t *testing.T) {
	h := newHarness(t)
	h.store.plans = map[int64]Plan{101: {Source: PlanSourceCache, Capture: "auto_explain",
		CapturedAt: t0.Add(-90 * time.Second),
		JSON:       []byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"orders"}}]`)}}
	out := call(t, ordersToolbox(t, h), "explain", `{"queryid":101}`)
	if out["source"] != PlanSourceCache || out["capture"] != "auto_explain" ||
		out["captured_at"] != "2026-10-04T11:58:30Z" || out["age_seconds"] != float64(90) {
		t.Fatalf("explain must carry source, captured_at and age_seconds: %v", out)
	}
}

func TestTools_ExplainOfAFreshPlanHasAgeZero(t *testing.T) {
	h := newHarness(t)
	h.store.plans = map[int64]Plan{101: {Source: PlanSourceGeneric, CapturedAt: t0,
		JSON: []byte(`[{"Plan":{"Node Type":"Seq Scan"}}]`)}}
	out := call(t, ordersToolbox(t, h), "explain", `{"queryid":101}`)
	if out["source"] != PlanSourceGeneric || out["age_seconds"] != float64(0) ||
		out["captured_at"] != "2026-10-04T12:00:00Z" || out["capture"] != nil {
		t.Fatalf("a plan made now: %v", out)
	}
}

func TestTools_ExplainWithoutPlanNamesWhyTheCacheWasSkipped(t *testing.T) {
	h := newHarness(t)
	h.store.plans = map[int64]Plan{101: {Source: PlanSourceNone,
		StaleReason: "captured 30h0m0s ago (limit 24h0m0s)"}}
	out := call(t, ordersToolbox(t, h), "explain", `{"queryid":101}`)
	if out["source"] != PlanSourceNone || out["stale_reason"] == nil ||
		out["captured_at"] != nil {
		t.Fatalf("no plan: source none and the stale reason: %v", out)
	}
}
