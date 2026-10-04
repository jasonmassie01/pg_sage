package tuning

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// What the agent did not examine keeps its open findings exactly as they
// were (lifeos, v1.9.0: a HypoPG-verified index on a table the exhausted
// budget never reached was resolved by absence). Only deterministic
// evidence resolves an open finding here: its table is gone, the index it
// would create exists or is covered, the index it would drop is gone.

func verifiedMemoriesIndex() analyzer.Finding {
	return analyzer.Finding{Category: optimizer.OptimizerCategory, Severity: "warning",
		ObjectType: "index", ObjectIdentifier: "public.memories|btree(user_id)",
		Title: "Missing index on public.memories",
		Detail: map[string]any{"table": "public.memories", "plan_source": "hypopg",
			"improvement_pct": 62.5},
		Recommendation: "index the user lookup",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY memories_user_idx " +
			"ON public.memories (user_id)",
		RollbackSQL: "DROP INDEX CONCURRENTLY public.memories_user_idx",
		ActionRisk:  "safe"}
}

func tuneN(t *testing.T, h *harness, cycles int) [][]analyzer.Finding {
	t.Helper()
	var outs [][]analyzer.Finding
	for i := 0; i < cycles; i++ {
		prev, cur := threeStatementPair()
		out, err := h.agent.Tune(context.Background(), cur, prev)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		outs = append(outs, out.Findings)
	}
	return outs
}

func byIdent(fs []analyzer.Finding, ident string) (analyzer.Finding, bool) {
	for _, f := range fs {
		if f.ObjectIdentifier == ident {
			return f, true
		}
	}
	return analyzer.Finding{}, false
}

func TestTune_BudgetExhaustedKeepsAnUnexaminedVerifiedRecommendation(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxRequestsPerCycle = 1
	h := newHarnessWith(t, s)
	open := verifiedMemoriesIndex()
	h.store.open = []analyzer.Finding{open}
	for i, fs := range tuneN(t, h, 2) {
		got, ok := byIdent(fs, open.ObjectIdentifier)
		if !ok {
			t.Fatalf("cycle %d: the verified recommendation on an unexamined table was "+
				"not re-emitted, so the analyzer would resolve it", i)
		}
		if !reflect.DeepEqual(got, open) {
			t.Fatalf("cycle %d: re-emitted changed:\n got %+v\nwant %+v", i, got, open)
		}
		if _, gated := got.Detail[analyzer.DetailApprovalRequired]; gated {
			t.Fatalf("cycle %d: it stays executable as it was", i)
		}
	}
	st := h.agent.Stats()
	if st.RequestsUsed != 1 || st.RequestLimit != 1 || st.CasesDeferred != 2 ||
		st.CasesAsked != 1 {
		t.Fatalf("stats = %+v: 1 of 1 requests, 1 case asked, 2 deferred", st)
	}
	if !h.logs.contains("deferred") || !h.logs.contains("top_statement:203") {
		t.Fatal("the deferred cases are logged by id")
	}
}

func TestTune_OpenFindingOfAVanishedCaseIsKept(t *testing.T) {
	h := newHarness(t)
	open := agentFinding("top_statement:555", optimizer.OptimizerCategory,
		"public.orders|btree(status)")
	h.store.open = []analyzer.Finding{open}
	out := tune(t, h)
	got, ok := byIdent(out.Findings, open.ObjectIdentifier)
	if !ok || !reflect.DeepEqual(got, open) {
		t.Fatalf("a case not seen this cycle keeps its open finding unchanged: %+v",
			out.Findings)
	}
}

func TestTune_LegacyAdvisorFindingIsKept(t *testing.T) {
	h := newHarness(t)
	open := analyzer.Finding{Category: "memory_tuning", Severity: "info",
		ObjectType: "setting", ObjectIdentifier: "work_mem", Title: "work_mem",
		RecommendedSQL: "ALTER SYSTEM SET work_mem = '64MB'"}
	h.store.open = []analyzer.Finding{open}
	if _, ok := byIdent(tune(t, h).Findings, "work_mem"); !ok {
		t.Fatal("an open finding the agent never examined is not resolved by absence")
	}
}

func TestTune_DeterministicEvidenceResolves(t *testing.T) {
	h := newHarness(t)
	gone := verifiedMemoriesIndex()
	gone.ObjectIdentifier = "public.gone|btree(a)"
	gone.Detail = map[string]any{"table": "public.gone", "plan_source": "hypopg"}
	covered := agentFinding("top_statement:555", optimizer.OptimizerCategory,
		"public.orders|btree(customer_id)")
	covered.RecommendedSQL = "CREATE INDEX CONCURRENTLY orders_c_idx ON public.orders " +
		"(customer_id)"
	dropped := analyzer.Finding{Category: CategoryIndexDrop, ObjectType: "index",
		ObjectIdentifier: "public.old_idx", RecommendedSQL: "DROP INDEX CONCURRENTLY " +
			"public.old_idx", Detail: map[string]any{"producer": Producer,
			"case_id": "write_amplification:public.orders", "table": "public.orders",
			"index": "public.old_idx"}}
	reloption := analyzer.Finding{Category: "vacuum_tuning", ObjectType: "table",
		ObjectIdentifier: "public.orders", RecommendedSQL: "ALTER TABLE public.orders SET " +
			"(autovacuum_vacuum_scale_factor = 0.02)", Detail: map[string]any{
			"producer": Producer, "case_id": "write_amplification:public.orders",
			"table": "public.orders"}}
	h.store.open = []analyzer.Finding{gone, covered, dropped, reloption}
	h.store.catalog = &CatalogState{
		Tables:  map[string]bool{"public.orders": true, "public.gone": false},
		Indexes: map[string]bool{"public.old_idx": false},
		IndexDefs: map[string][]string{"public.orders": {
			"CREATE INDEX orders_cs ON public.orders USING btree (customer_id, status)"}}}
	out := tune(t, h)
	for _, ident := range []string{gone.ObjectIdentifier, covered.ObjectIdentifier,
		dropped.ObjectIdentifier} {
		if _, ok := byIdent(out.Findings, ident); ok {
			t.Errorf("%s: deterministic evidence resolves it", ident)
		}
	}
	if _, ok := byIdent(out.Findings, reloption.ObjectIdentifier); !ok {
		t.Error("a finding with no contrary evidence is kept")
	}
}

func TestTune_CatalogUnreadableKeepsEveryOpenFinding(t *testing.T) {
	h := newHarness(t)
	gone := verifiedMemoriesIndex()
	h.store.open = []analyzer.Finding{gone}
	h.store.catalog = &CatalogState{Tables: map[string]bool{"public.memories": false}}
	h.store.catErr = errFake
	if _, ok := byIdent(tune(t, h).Findings, gone.ObjectIdentifier); !ok {
		t.Fatal("without catalog evidence nothing is resolved")
	}
	if !h.logs.contains("catalog") {
		t.Fatal("the unreadable catalog is logged")
	}
}

// askedQueryIDs reports, per model call, which case statement it was about.
func askedQueryIDs(m *scriptedModel) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msgs := range m.msgs {
		for _, id := range []string{"201", "202", "203"} {
			for _, msg := range msgs {
				if strings.Contains(msg.Content, "queryid "+id+" [") {
					out = append(out, id)
					break
				}
			}
		}
	}
	return out
}

func TestTune_DeferredCasesAreAskedBeforeRepeats(t *testing.T) {
	s := defaultSettings()
	s.Tuning.MaxCasesPerCycle = 1
	s.Memory.SkipLLMAfter = 100 // empty answers must not hide a case here
	h := newHarnessWith(t, s)
	tuneN(t, h, 4)
	got := strings.Join(askedQueryIDs(h.model), ",")
	if got != "201,202,203,201" {
		t.Fatalf("asked %s: the most valuable case first, then each deferred case in "+
			"turn before any case is asked again", got)
	}
}
