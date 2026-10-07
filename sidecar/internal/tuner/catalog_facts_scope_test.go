package tuner

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// The tuner reads catalog facts only for the relations its plans name
// (perf gate: one whole-catalog read per cycle cost 136 ms at 5,000
// tables, every cycle, with or without candidates).

func TestCatalogFactsSQL_ScopedToNamedRelations(t *testing.T) {
	if !strings.Contains(catalogFactsSQL, "c.relname = ANY($1") {
		t.Fatalf("catalog facts read is not scoped to the plans' relations:\n%s",
			catalogFactsSQL)
	}
}

// No relation named: nothing to read, and the database is not asked (a
// canceled context would fail any query).
func TestLoadCatalogFacts_NoNamesReadsNothing(t *testing.T) {
	pool, _ := requireTunerDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, names := range [][]string{nil, {}, {""}} {
		facts, err := LoadCatalogFacts(ctx, pool, names)
		if err != nil || facts == nil || len(facts.Tables) != 0 {
			t.Fatalf("names %q = %+v %v, want empty facts and no query", names, facts, err)
		}
	}
}

func TestPlanRelationNames(t *testing.T) {
	plan := `[{"Plan": {"Node Type": "Nested Loop", "Plans": [
		{"Node Type": "Seq Scan", "Relation Name": "orders", "Schema": "app"},
		{"Node Type": "Index Scan", "Relation Name": "users"},
		{"Node Type": "Seq Scan", "Relation Name": "orders"}]}}]`
	got, err := planRelationNames([]byte(plan))
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if strings.Join(got, ",") != "orders,users" {
		t.Fatalf("names = %v, want [orders users] (sorted, deduplicated)", got)
	}
	if names, err := planRelationNames([]byte(`[{"Plan": {"Node Type": "Result"}}]`)); err != nil ||
		len(names) != 0 {
		t.Fatalf("plan without relations = %v %v, want none", names, err)
	}
	if _, err := planRelationNames([]byte(`{not json`)); err == nil {
		t.Fatal("malformed plan gave no error")
	}
}

// A relation is read once per cycle: a second plan naming it reuses the
// facts (the table dropped in between is still known), a new name is read.
func TestTunerCycleFacts_ReadsEachRelationOncePerCycle(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	for _, s := range []string{"DROP SCHEMA IF EXISTS factscope CASCADE",
		"CREATE SCHEMA factscope",
		"CREATE TABLE factscope.first (id int PRIMARY KEY)",
		"CREATE TABLE factscope.second (id int PRIMARY KEY)"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS factscope CASCADE")
	})
	tu := &Tuner{pool: pool, logFn: func(string, string, ...any) {}}
	tu.loadFacts(ctx)
	plan := func(rel string) []byte {
		return []byte(`[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "` + rel +
			`", "Schema": "factscope"}}]`)
	}
	if f := tu.cycleFacts(ctx, plan("first")); f == nil {
		t.Fatal("no facts for the first plan")
	} else if _, ok := f.TableRows("factscope", "first"); !ok {
		t.Fatalf("first not loaded: %+v", f.Tables)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE factscope.first"); err != nil {
		t.Fatal(err)
	}
	f := tu.cycleFacts(ctx, plan("first"))
	if _, ok := f.TableRows("factscope", "first"); !ok {
		t.Fatal("first was read again within the cycle")
	}
	if _, ok := f.TableRows("factscope", "second"); ok {
		t.Fatal("second loaded before any plan named it")
	}
	f = tu.cycleFacts(ctx, plan("second"))
	if _, ok := f.TableRows("factscope", "second"); !ok {
		t.Fatal("second not loaded when a plan named it")
	}
	tu.loadFacts(ctx) // a new cycle forgets what the last one read
	f = tu.cycleFacts(ctx, plan("first"))
	if _, ok := f.TableRows("factscope", "first"); ok {
		t.Fatal("a new cycle reused facts of a dropped table")
	}
}

// A failed read skips the catalog heuristics for that plan and says why.
func TestTunerCycleFacts_ReadErrorSkipsHeuristics(t *testing.T) {
	pool, _ := requireTunerDB(t)
	var mu sync.Mutex
	var logs []string
	tu := &Tuner{pool: pool, logFn: func(level, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, level+" "+format)
	}}
	tu.loadFacts(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := []byte(`[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "x"}}]`)
	if f := tu.cycleFacts(ctx, plan); f != nil {
		t.Fatalf("facts after a failed read = %+v, want nil", f)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "WARN") ||
		!strings.Contains(logs[0], "skipped") {
		t.Fatalf("logs = %q, want one WARN saying the checks are skipped", logs)
	}
}

// Without a pool (unit tests, degraded runtime) there are no facts and
// no error.
func TestTunerCycleFacts_NilPool(t *testing.T) {
	tu := &Tuner{logFn: func(string, string, ...any) {}}
	tu.loadFacts(context.Background())
	plan := []byte(`[{"Plan": {"Node Type": "Seq Scan", "Relation Name": "x"}}]`)
	f := tu.cycleFacts(context.Background(), plan)
	if f == nil || len(f.Tables) != 0 {
		t.Fatalf("nil pool facts = %+v, want empty", f)
	}
}
