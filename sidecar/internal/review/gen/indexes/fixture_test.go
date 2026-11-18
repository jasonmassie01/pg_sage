package indexes

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// fact is a valid, droppable btree index of public.<table>.
func fact(table, name, cols string, size int64, prefixOf ...string) workpack.IndexFact {
	if prefixOf == nil {
		prefixOf = []string{}
	}
	return workpack.IndexFact{ID: "index/idx/public." + name, Schema: "public", Table: table,
		Index: name, Method: "btree", SizeBytes: size, Valid: true,
		Definition: fmt.Sprintf("CREATE INDEX %s ON public.%s USING btree (%s)", name,
			table, cols),
		Scans: int64(5), TableWrites: workpack.Writes{Inserts: 900, Updates: 900},
		DuplicateOf: []string{}, PrefixOf: prefixOf}
}

// chainFacts is (a), (a,b) and (a,b,c) on public.t.
func chainFacts() []workpack.IndexFact {
	return []workpack.IndexFact{
		fact("t", "t_a", "a", 1000, "public.t_ab", "public.t_abc"),
		fact("t", "t_ab", "a, b", 2000, "public.t_abc"),
		fact("t", "t_abc", "a, b, c", 3000),
	}
}

// spec is a pack of db over days with facts, one family reading public.t
// (queryids 7 and 9) and its texts.
func spec(db string, days int, status string, facts ...workpack.IndexFact) reviewtest.Spec {
	return reviewtest.Spec{Database: db, Window: reviewtest.Window(days),
		Workload: workpack.Workload{Families: []workpack.Family{{ID: "workload/family/f1",
			Relations: []string{"public.t"}, QueryIDs: []workpack.Query{
				{ID: "workload/query/7", QueryID: "7", Calls: 10, ExecMs: 50, TextKnown: true},
				{ID: "workload/query/9", QueryID: "9", Calls: 5, ExecMs: 900,
					TextKnown: true}}}}},
		Texts: &workpack.Texts{Texts: []workpack.QueryText{
			{QueryID: "7", Text: "SELECT * FROM public.t WHERE a = $1"},
			{QueryID: "9", Text: "SELECT * FROM t WHERE a = $1 AND b = $2"}}},
		Indexes: &workpack.Indexes{ScansStatus: status, Indexes: facts,
			Total: len(facts)}}
}

func mustPack(t *testing.T, s reviewtest.Spec) *workpack.Pack {
	t.Helper()
	p, err := reviewtest.Build(s)
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

func env(db string) candidate.Env {
	return candidate.Env{Database: db, WindowDays: 90, MinImprovementPct: 10,
		MaxIndexesPerTable: 10}
}

// fakeMeasurer answers every drop set with res and err, recording each.
type fakeMeasurer struct {
	mu   sync.Mutex
	res  optimizer.WhatIfResult
	err  error
	sets []DropSet
}

func (f *fakeMeasurer) MeasureDropSet(_ context.Context, s DropSet,
) (optimizer.WhatIfResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, s)
	return f.res, f.err
}

// measured is a complete measurement without regressions.
func measured() *fakeMeasurer {
	return &fakeMeasurer{res: optimizer.WhatIfResult{Measured: 2, Guarded: 2}}
}

func byTarget(cands []candidate.Candidate) map[string]candidate.Candidate {
	out := map[string]candidate.Candidate{}
	for _, c := range cands {
		out[c.Target] = c
	}
	return out
}
