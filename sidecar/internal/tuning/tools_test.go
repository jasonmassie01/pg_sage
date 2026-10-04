package tuning

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The read-only tools the model may call during a case. Each successful
// result is registered as citable evidence (R1, R2, ...); a refusal is an
// error result and registers nothing.

func ordersToolbox(t *testing.T, h *harness) *toolbox {
	t.Helper()
	prev, cur := ordersPair()
	w := ClassifyWorkload(cur, nil, t0)
	cs := DetectCases(cur, prev, w, DefaultThresholds())
	return h.agent.newToolbox(cs[0], cur, prev, w, h.agent.cycleTools())
}

func call(t *testing.T, tb *toolbox, name, args string) map[string]any {
	t.Helper()
	raw := tb.exec(context.Background(), llm.ToolCall{ID: "c", Name: name,
		Arguments: json.RawMessage(args)})
	inner := raw
	if i := strings.Index(raw, ">"); strings.HasPrefix(raw, "<data") && i > 0 {
		inner = strings.TrimSuffix(strings.TrimSpace(raw[i+1:]), "</data>")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(inner), &out); err != nil {
		t.Fatalf("tool %s returned non-JSON %q: %v", name, raw, err)
	}
	return out
}

func TestTools_StatementRegistersEvidence(t *testing.T) {
	h := newHarness(t)
	tb := ordersToolbox(t, h)
	out := call(t, tb, "statement", `{"queryid":101}`)
	if out["evidence_id"] != "R1" || out["error"] != nil {
		t.Fatalf("result = %v", out)
	}
	interval, _ := out["interval"].(map[string]any)
	if interval["calls"] != float64(600) {
		t.Fatalf("interval = %v", out["interval"])
	}
	ev := tb.evidence["R1"]
	if ev.Kind != "tool:statement" || ev.Ref != "queryid:101" {
		t.Fatalf("evidence = %+v", ev)
	}
	second := call(t, tb, "statement", `{"queryid":"102"}`)
	if second["evidence_id"] != "R2" {
		t.Fatalf("numbering continues, string queryids accepted: %v", second)
	}
}

func TestTools_RefusalsRegisterNothing(t *testing.T) {
	h := newHarness(t)
	tb := ordersToolbox(t, h)
	for name, args := range map[string]string{
		"statement": `{"queryid":424242}`,
		"table":     `{"table":"sage.findings"}`,
		"nope":      `{}`,
	} {
		out := call(t, tb, name, args)
		if out["error"] == nil || out["evidence_id"] != nil {
			t.Fatalf("%s %s: %v", name, args, out)
		}
	}
	out := call(t, tb, "statement", `{"queryid":`)
	if out["error"] == nil {
		t.Fatalf("malformed arguments: %v", out)
	}
	if len(tb.evidence) != 0 {
		t.Fatalf("evidence = %v", tb.evidence)
	}
}

func TestTools_TableShowsColumnsIndexesAndWrites(t *testing.T) {
	h := newHarness(t)
	tb := ordersToolbox(t, h)
	out := call(t, tb, "table", `{"table":"public.orders"}`)
	raw, _ := json.Marshal(out)
	for _, want := range []string{"customer_id", "orders_pkey", "live_tuples", "R1"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("table result lacks %q: %s", want, raw)
		}
	}
}

func TestTools_ExplainUsesThePlanSource(t *testing.T) {
	h := newHarness(t)
	h.store.plans = map[int64]Plan{101: {Source: PlanSourceCache,
		JSON: []byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"orders",` +
			`"Plan Rows":5,"Filter":"(customer_id = $1)"}}]`)}}
	tb := ordersToolbox(t, h)
	out := call(t, tb, "explain", `{"queryid":101}`)
	if out["source"] != PlanSourceCache || !strings.Contains(out["plan"].(string), "Seq Scan") {
		t.Fatalf("explain = %v", out)
	}
	none := call(t, tb, "explain", `{"queryid":102}`)
	if none["source"] != PlanSourceNone || none["evidence_id"] != nil {
		t.Fatalf("no plan is not evidence: %v", none)
	}
}

func TestTools_WhatIfUsesTheOptimizerAndIsCapped(t *testing.T) {
	h := newHarness(t)
	tb := ordersToolbox(t, h)
	ddl := `{"ddl":"CREATE INDEX CONCURRENTLY w ON public.orders (customer_id)"}`
	out := call(t, tb, "whatif_index", ddl)
	if out["verdict"] != "verified" || out["improvement_pct"] != float64(40) ||
		out["evidence_id"] != "R1" {
		t.Fatalf("whatif = %v", out)
	}
	for i := 0; i < maxWhatIfPerCase-1; i++ {
		call(t, tb, "whatif_index", ddl)
	}
	capped := call(t, tb, "whatif_index", ddl)
	if capped["error"] == nil {
		t.Fatalf("what-ifs per case are capped at %d: %v", maxWhatIfPerCase, capped)
	}
	if got := len(h.indexes.admittedDDL()); got != maxWhatIfPerCase {
		t.Fatalf("what-ifs run = %d", got)
	}
	h.indexes.admit = func(rec optimizer.Recommendation) optimizer.Admission {
		return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitRejected,
			Reason: "call-weighted improvement 2.0% is below the 10.0% minimum"}
	}
	tb = ordersToolbox(t, h)
	rejected := call(t, tb, "whatif_index", ddl)
	if rejected["verdict"] != "rejected" || !strings.Contains(rejected["reason"].(string), "2.0%") {
		t.Fatalf("a rejection is reported (and is evidence too): %v", rejected)
	}
	other := call(t, tb, "whatif_index", `{"ddl":"CREATE INDEX x ON test_abc.orders (id)"}`)
	if other["error"] == nil {
		t.Fatalf("non-workload tables are refused: %v", other)
	}
}

func TestTools_WriteCostAndExtendedStats(t *testing.T) {
	h := newHarness(t)
	h.store.colStats = []ColumnStat{{Column: "customer_id", AvgWidth: 8, NDistinct: -0.1},
		{Column: "status", AvgWidth: 6, NDistinct: 5, Correlation: 0.2}}
	h.store.extStats = []ExtStat{{Name: "orders_stx", Columns: []string{"a", "b"},
		Kinds: []string{"ndistinct"}}}
	tb := ordersToolbox(t, h)
	wc := call(t, tb, "write_cost", `{"table":"public.orders","columns":["customer_id"]}`)
	if wc["entry_bytes"] != float64(24) || wc["evidence_id"] == nil {
		t.Fatalf("write cost = %v", wc)
	}
	es := call(t, tb, "extended_stats", `{"table":"public.orders",`+
		`"columns":["customer_id","status"]}`)
	raw, _ := json.Marshal(es)
	if !strings.Contains(string(raw), "orders_stx") || !strings.Contains(string(raw),
		"n_distinct") {
		t.Fatalf("extended stats = %s", raw)
	}
}

func TestTools_ResultsAreBoundedUntrustedData(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("x", 20000)
	h.store.plans = map[int64]Plan{101: {Source: PlanSourceCache,
		JSON: []byte(`[{"Plan":{"Node Type":"Seq Scan","Filter":"` + big + `"}}]`)}}
	tb := ordersToolbox(t, h)
	raw := tb.exec(context.Background(), llm.ToolCall{ID: "c", Name: "explain",
		Arguments: json.RawMessage(`{"queryid":101}`)})
	if !strings.HasPrefix(raw, "<data") || len(raw) > maxToolResultBytes+256 {
		t.Fatalf("result %d bytes, prefix %q", len(raw), raw[:min(len(raw), 20)])
	}
}

func TestTools_RehearseOnlyWithAProvider(t *testing.T) {
	h := newHarness(t)
	tb := ordersToolbox(t, h)
	if out := call(t, tb, "rehearse", `{"ddl":"CREATE INDEX CONCURRENTLY r ON `+
		`public.orders (status)"}`); out["error"] == nil {
		t.Fatalf("no provider: %v", out)
	}
	if specsHave(h.agent.toolSpecs(), "rehearse") {
		t.Fatal("the rehearse tool is offered only with a clone provider")
	}
	reh := &fakeRehearser{res: RehearsalResult{BuildMs: 120, SizeDeltaBytes: 4096,
		Queries: []RehearsedQuery{{QueryID: 101, BeforeCost: 1000, AfterCost: 12}}}}
	h.agent.deps.Rehearse = reh
	if !specsHave(h.agent.toolSpecs(), "rehearse") {
		t.Fatal("with a provider the tool is offered")
	}
	tb = ordersToolbox(t, h)
	out := call(t, tb, "rehearse", `{"ddl":"CREATE INDEX CONCURRENTLY r ON public.orders (status)"}`)
	if out["build_ms"] != float64(120) || out["evidence_id"] != "R1" {
		t.Fatalf("rehearsal = %v", out)
	}
	if len(reh.got) != 1 || len(reh.got[0].Statements) != 1 ||
		reh.got[0].Statements[0].QueryID != 101 {
		t.Fatalf("the case statements are rehearsed: %+v", reh.got)
	}
	again := call(t, tb, "rehearse",
		`{"ddl":"CREATE INDEX CONCURRENTLY r2 ON public.orders (status)"}`)
	if again["error"] == nil {
		t.Fatalf("one rehearsal per cycle: %v", again)
	}
}

type fakeRehearser struct {
	res RehearsalResult
	err error
	got []RehearsalRequest
}

func (f *fakeRehearser) Rehearse(_ context.Context, r RehearsalRequest) (RehearsalResult, error) {
	f.got = append(f.got, r)
	return f.res, f.err
}

func specsHave(specs []llm.ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}
