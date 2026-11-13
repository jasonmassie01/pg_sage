package analyzer

import (
	"reflect"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-11 rule packs, without a database.

func rulePackAnalyzer(global ...string) *Analyzer {
	cfg := phase2Config()
	cfg.Analyzer.DisabledRules = global
	a := New(nil, cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	return a
}

func TestDisabledRules_SnapshotRuleIsSkippedNotFailed(t *testing.T) {
	a := rulePackAnalyzer("unused_indexes")
	a.runSnapshotRules(&collector.Snapshot{}, nil, false)
	if a.eval.ok["unused_index"] || a.eval.failed["unused_index"] {
		t.Fatalf("unused_index marked ok=%v failed=%v; a disabled rule must not run",
			a.eval.ok["unused_index"], a.eval.failed["unused_index"])
	}
	if !a.eval.failed["invalid_index"] {
		t.Fatal("invalid_index lacks input and must still be marked failed")
	}
	if !a.eval.disabled["unused_index"] {
		t.Fatalf("disabled = %v, want unused_index", a.eval.disabled)
	}
}

func TestDisabledRules_FindingsDroppedAndNotResolvable(t *testing.T) {
	a := rulePackAnalyzer("slow_query")
	in := []Finding{
		lifecycleFinding("slow_query", "q1", "warning"),
		lifecycleFinding("unused_index", "public.i", "info"),
	}
	out := a.applyDisabledRules(in)
	if len(out) != 1 || out[0].Category != "unused_index" {
		t.Fatalf("applyDisabledRules = %+v, want only the unused_index finding", out)
	}
	a.eval.evaluated("slow_query", "unused_index")
	res := a.eval.resolvable(out)
	if res["slow_query"] || !res["unused_index"] {
		t.Fatalf("resolvable = %v, want slow_query excluded", res)
	}
}

// Nil/empty: no disabled rules is exactly today's behaviour.
func TestDisabledRules_EmptyKeepsGoldenFindingSet(t *testing.T) {
	a := rulePackAnalyzer()
	in := []Finding{
		lifecycleFinding("slow_query", "q1", "warning"),
		lifecycleFinding("wraparound_freeze", "public.t", "critical"),
		lifecycleFinding("custom:no_pk", "public.t", "warning"),
	}
	golden := append([]Finding(nil), in...)
	if out := a.applyDisabledRules(in); !reflect.DeepEqual(out, golden) {
		t.Fatalf("applyDisabledRules changed the set: %+v", out)
	}
	if len(a.eval.disabled) != 0 {
		t.Fatalf("disabled = %v, want none", a.eval.disabled)
	}
	if got := a.runCustomChecks(nil); got != nil {
		t.Fatalf("runCustomChecks without checks = %+v, want nil", got)
	}
}

func TestDisabledRules_OverrideReplacesGlobalAndClearInherits(t *testing.T) {
	a := rulePackAnalyzer("slow_query")
	a.SetDisabledRules([]string{"unused_indexes"}, "alice")
	cats, by := a.disabledCategories()
	if !cats["unused_index"] || cats["slow_query"] || by != "alice" {
		t.Fatalf("override = %v by %q, want unused_index by alice", cats, by)
	}
	if names, ok := a.DisabledRulesOverride(); !ok || len(names) != 1 {
		t.Fatalf("DisabledRulesOverride = %v, %v", names, ok)
	}
	a.SetDisabledRules([]string{}, "alice")
	if cats, _ := a.disabledCategories(); len(cats) != 0 {
		t.Fatalf("empty override = %v, want nothing disabled", cats)
	}
	a.ClearDisabledRules()
	cats, by = a.disabledCategories()
	if !cats["slow_query"] || by == "" {
		t.Fatalf("after clear = %v by %q, want the global slow_query", cats, by)
	}
	if _, ok := a.DisabledRulesOverride(); ok {
		t.Fatal("override still reported after ClearDisabledRules")
	}
}

// A protected name that reached the runtime (a hand-edited override row)
// is ignored: the rule keeps running.
func TestDisabledRules_ProtectedIgnoredAtRuntime(t *testing.T) {
	a := rulePackAnalyzer()
	a.SetDisabledRules([]string{"wraparound_freeze", "unused_indexes"}, "x")
	cats, _ := a.disabledCategories()
	if cats["wraparound_freeze"] || !cats["unused_index"] {
		t.Fatalf("disabled = %v, want unused_index only", cats)
	}
}

func TestDisabledRules_ConcurrentSetAndRead(t *testing.T) {
	a := rulePackAnalyzer()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			a.SetDisabledRules([]string{"unused_indexes"}, "x")
		}()
		go func() {
			defer wg.Done()
			cats, _ := a.disabledCategories()
			if len(cats) > 1 {
				t.Errorf("torn read: %v", cats)
			}
		}()
	}
	wg.Wait()
	if cats, _ := a.disabledCategories(); !cats["unused_index"] {
		t.Fatalf("final = %v", cats)
	}
}
