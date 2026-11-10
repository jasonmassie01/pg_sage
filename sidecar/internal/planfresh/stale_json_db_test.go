package planfresh

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// tuning-08 review: a stale plan's relations still reach the tuner's
// deferral gate (StaleJSON), never its shape (JSON), and a plan from the
// EXPLAIN fallback never claims actuals. plan_json is jsonb, so a stored
// plan is compared by the schema it names, not byte for byte.

func TestReader_StaleJSONCarriesTheNewestStalePlanOnly(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "31 hours", `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"old"}}]`,
		"auto_explain")
	cache(t, pool, qid, "30 hours", planOn(s), "auto_explain")
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceNone || len(p.JSON) != 0 ||
		!strings.Contains(string(p.StaleJSON), `"`+s+`"`) ||
		!strings.Contains(p.StaleReason, "old") {
		t.Fatalf("every capture stale, no fallback: no plan, the newest stale one in "+
			"StaleJSON: %+v, %v", p, err)
	}
	if prov := p.Provenance(t0); prov["stale_json"] != nil || len(prov) != 2 {
		t.Fatalf("provenance must not carry the stale plan: %v", prov)
	}
}

func TestReader_FreshPlanHasNoStaleJSON(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "-1 second", planOn(s), "auto_explain")
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceCache || p.StaleJSON != nil {
		t.Fatalf("a fresh plan is used and no stale plan rides along: %+v, %v", p, err)
	}
}

func TestReader_NoCacheHasNoStaleJSON(t *testing.T) {
	pool := dbPool(t)
	_, qid := fixture(t, pool)
	p, err := reader(pool).Read(context.Background(), qid, "")
	if err != nil || p.Source != SourceNone || p.StaleJSON != nil || p.StaleReason != "" {
		t.Fatalf("no cache row: nothing stale to report: %+v, %v", p, err)
	}
}

func TestReader_ExplainFallbackNeverClaimsActuals(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "30 hours", planOn(s), "auto_explain")
	r := reader(pool)
	r.Explain = func(context.Context, string) (Plan, error) {
		return Plan{Source: SourceExplain, HasActuals: true,
			JSON: []byte(`[{"Plan":{"Node Type":"Seq Scan","Actual Rows":5}}]`)}, nil
	}
	p, err := r.Read(context.Background(), qid, "SELECT 1")
	if err != nil || p.Source != SourceExplain || p.HasActuals ||
		!strings.Contains(string(p.StaleJSON), `"`+s+`"`) {
		t.Fatalf("a fallback plan never carries actuals and keeps the stale relations: "+
			"%+v, %v", p, err)
	}
}

func TestReader_FailingExplainReturnsNoStaleJSON(t *testing.T) {
	pool := dbPool(t)
	s, qid := fixture(t, pool)
	cache(t, pool, qid, "30 hours", planOn(s), "auto_explain")
	r := reader(pool)
	r.Explain = func(context.Context, string) (Plan, error) {
		return Plan{}, errors.New("permission denied for table t")
	}
	p, err := r.Read(context.Background(), qid, "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "permission denied") ||
		len(p.JSON) != 0 || len(p.StaleJSON) != 0 {
		t.Fatalf("a failed EXPLAIN is an error with no plan of any kind: %+v, %v", p, err)
	}
}
