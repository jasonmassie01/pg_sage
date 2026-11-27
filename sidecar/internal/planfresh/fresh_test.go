package planfresh

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// No concurrent access tests in this file: Fresh, HasActuals, PlanNames and
// MaxAge are pure functions over their arguments.

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func capturedAt(at time.Time) Capture {
	return Capture{JSON: []byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t"}}]`),
		Source: "auto_explain", CapturedAt: at}
}

func TestFresh_AnalyzeBoundary(t *testing.T) {
	now := t0.Add(time.Hour)
	c := capturedAt(t0)
	if v := Fresh(c, Events{Analyzed: t0.Add(time.Second)}, now, DefaultMaxAge); v.Fresh ||
		!strings.Contains(v.Reason, "ANALYZE") {
		t.Fatalf("a plan 1s before last_autoanalyze must be stale: %+v", v)
	}
	if v := Fresh(c, Events{Analyzed: t0.Add(-time.Second)}, now, DefaultMaxAge); !v.Fresh ||
		v.Reason != "" {
		t.Fatalf("a plan 1s after the last ANALYZE must be fresh: %+v", v)
	}
	if v := Fresh(c, Events{Analyzed: t0}, now, DefaultMaxAge); v.Fresh {
		t.Fatalf("a plan captured at the ANALYZE instant cannot be shown to follow it: %+v", v)
	}
}

func TestFresh_AgeEqualToTheLimitIsStale(t *testing.T) {
	c := capturedAt(t0)
	if v := Fresh(c, Events{}, t0.Add(DefaultMaxAge), DefaultMaxAge); v.Fresh ||
		!strings.Contains(v.Reason, "old") {
		t.Fatalf("age == limit must be stale: %+v", v)
	}
	if v := Fresh(c, Events{}, t0.Add(DefaultMaxAge-time.Second), DefaultMaxAge); !v.Fresh {
		t.Fatalf("age one second under the limit must be fresh: %+v", v)
	}
	limit := 30 * time.Minute
	if v := Fresh(c, Events{}, t0.Add(limit), limit); v.Fresh {
		t.Fatalf("a configured 30m limit must apply: %+v", v)
	}
}

func TestFresh_PgSageActionAfterCaptureIsStale(t *testing.T) {
	now := t0.Add(time.Hour)
	c := capturedAt(t0)
	v := Fresh(c, Events{Action: t0.Add(time.Minute)}, now, DefaultMaxAge)
	if v.Fresh || !strings.Contains(v.Reason, "pg_sage action") {
		t.Fatalf("a CREATE INDEX by pg_sage after capture must make the plan stale: %+v", v)
	}
	if v := Fresh(c, Events{Action: t0.Add(-time.Minute)}, now, DefaultMaxAge); !v.Fresh {
		t.Fatalf("an action before capture leaves it fresh: %+v", v)
	}
}

func TestFresh_NoEventsMeansTheAgeRuleOnly(t *testing.T) {
	c := capturedAt(t0)
	if v := Fresh(c, Events{}, t0.Add(time.Hour), DefaultMaxAge); !v.Fresh {
		t.Fatalf("no last_analyze (NULL) and no action: fresh within the age: %+v", v)
	}
	if v := Fresh(c, Events{}, t0.Add(25*time.Hour), DefaultMaxAge); v.Fresh {
		t.Fatalf("no events never extends the age limit: %+v", v)
	}
}

func TestFresh_InvalidCaptureIsStale(t *testing.T) {
	v := Fresh(Capture{JSON: []byte(`[]`)}, Events{}, t0, DefaultMaxAge)
	if v.Fresh || v.Reason == "" {
		t.Fatalf("a capture without a time is never fresh: %+v", v)
	}
}

func TestFresh_NonPositiveLimitMeansTheDefault(t *testing.T) {
	c := capturedAt(t0)
	for _, limit := range []time.Duration{0, -time.Hour} {
		if v := Fresh(c, Events{}, t0.Add(23*time.Hour), limit); !v.Fresh {
			t.Fatalf("limit %v must mean the 24h default (fresh at 23h): %+v", limit, v)
		}
		if v := Fresh(c, Events{}, t0.Add(DefaultMaxAge), limit); v.Fresh {
			t.Fatalf("limit %v must mean the 24h default (stale at 24h): %+v", limit, v)
		}
	}
}

func TestMaxAge(t *testing.T) {
	cases := map[int]time.Duration{0: DefaultMaxAge, -5: DefaultMaxAge, 1: time.Minute,
		30: 30 * time.Minute, 1440: 24 * time.Hour, 2880: 48 * time.Hour}
	for minutes, want := range cases {
		if got := MaxAge(minutes); got != want {
			t.Errorf("MaxAge(%d) = %v, want %v", minutes, got, want)
		}
	}
	if DefaultMaxAge != 24*time.Hour {
		t.Fatalf("DefaultMaxAge = %v, want 24h", DefaultMaxAge)
	}
}

func TestPlanNames(t *testing.T) {
	auto := []byte(`{"Query Text":"select","Plan":{"Node Type":"Nested Loop","Plans":[
		{"Node Type":"Index Scan","Relation Name":"Orders","Schema":"shop",
		 "Index Name":"orders_pkey"},
		{"Node Type":"Seq Scan","Relation Name":"items"}],
		"InitPlan":[{"Node Type":"Seq Scan","Relation Name":"items"}]}}`)
	n, err := PlanNames(auto)
	if err != nil {
		t.Fatalf("PlanNames: %v", err)
	}
	if !slices.Equal(n.Qualified, []string{"shop.Orders"}) ||
		!slices.Equal(n.Unqualified, []string{"items"}) ||
		!slices.Equal(n.Objects, []string{"items", "orders", "orders_pkey"}) {
		t.Fatalf("names = %+v", n)
	}
	arr, err := PlanNames([]byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t"}}]`))
	if err != nil || !slices.Equal(arr.Unqualified, []string{"t"}) || len(arr.Qualified) != 0 {
		t.Fatalf("array plan names = %+v %v", arr, err)
	}
}

func TestPlanNames_NoRelationAndInvalidInput(t *testing.T) {
	n, err := PlanNames([]byte(`[{"Plan":{"Node Type":"Result"}}]`))
	if err != nil || !n.Empty() {
		t.Fatalf("a plan naming no relation has no names: %+v %v", n, err)
	}
	for _, bad := range []string{"", "not json", `{"Plan":`} {
		if _, err := PlanNames([]byte(bad)); err == nil {
			t.Errorf("PlanNames(%q) must fail", bad)
		}
	}
}

func TestHasActuals(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		c    Capture
		want bool
	}{
		{"plain explain", Capture{JSON: []byte(`[{"Plan":{"Node Type":"Sort"}}]`)}, false},
		{"actual rows", Capture{JSON: []byte(`[{"Plan":{"Actual Rows":5}}]`)}, true},
		{"disk sort", Capture{JSON: []byte(`[{"Plan":{"Sort Space Type":"Disk"}}]`)}, true},
		{"hash batches", Capture{JSON: []byte(`[{"Plan":{"Hash Batches":4}}]`)}, true},
		{"provider analyze", Capture{JSON: []byte(`{}`), AnalyzeEnabled: &yes}, true},
		{"provider says no analyze", Capture{JSON: []byte(`[{"Plan":{"Actual Rows":5}}]`),
			AnalyzeEnabled: &no}, false},
		{"empty", Capture{}, false},
	}
	for _, c := range cases {
		if got := HasActuals(c.c); got != c.want {
			t.Errorf("%s: HasActuals = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlanProvenance(t *testing.T) {
	p := Plan{Source: SourceCache, Capture: "auto_explain", CapturedAt: t0}
	got := p.Provenance(t0.Add(90 * time.Second))
	if got["source"] != SourceCache || got["capture"] != "auto_explain" ||
		got["captured_at"] != "2026-11-10T12:00:00Z" || got["age_seconds"] != int64(90) {
		t.Fatalf("provenance = %v", got)
	}
	none := Plan{Source: SourceNone, StaleReason: "captured 30h ago"}.Provenance(t0)
	if none["source"] != SourceNone || none["captured_at"] != nil ||
		none["stale_reason"] != "captured 30h ago" {
		t.Fatalf("no-plan provenance = %v", none)
	}
}
