package analyzer

import (
	"fmt"
	"sync"
	"testing"
)

// Index definitions repeat every cycle (15,000 of them at the perf gate's
// large scale, 0.9 s of regexp per run), so parses are memoized. A cached
// result must behave exactly like a fresh parse.

func TestParseIndexDefCachedResultIsACopy(t *testing.T) {
	def := `CREATE INDEX idx_a ON public.t USING btree (a, b) INCLUDE (c) WHERE (a > 0)`
	first := ParseIndexDef(def)
	first.Columns[0] = "mutated"
	first.IncludeCols[0] = "mutated"
	again := ParseIndexDef(def)
	if again.Columns[0] != "a" || again.IncludeCols[0] != "c" {
		t.Fatalf("a caller's change leaked into the cache: %+v", again)
	}
	if again.Name != "idx_a" || again.Schema != "public" || again.Table != "t" ||
		again.WhereClause != "(a > 0)" || again.IndexType != "btree" {
		t.Fatalf("cached parse = %+v", again)
	}
}

func TestParseIndexDefUnparseableIsCachedAsEmpty(t *testing.T) {
	for i := 0; i < 2; i++ {
		if p := ParseIndexDef("not an index"); p.Name != "" || p.Columns != nil {
			t.Fatalf("pass %d: unparseable = %+v, want empty", i, p)
		}
	}
}

func TestIndexDefCacheIsBounded(t *testing.T) {
	for i := 0; i < indexDefCacheMax+100; i++ {
		ParseIndexDef(fmt.Sprintf("CREATE INDEX i%d ON public.t USING btree (c%d)", i, i))
	}
	if n := indexDefCacheLen(); n > indexDefCacheMax {
		t.Fatalf("cache holds %d definitions, cap %d", n, indexDefCacheMax)
	}
	p := ParseIndexDef("CREATE INDEX late ON public.t USING btree (z)")
	if p.Name != "late" || len(p.Columns) != 1 || p.Columns[0] != "z" {
		t.Fatalf("parse after the cap = %+v", p)
	}
}

// Analyzer rules parse concurrently (run with -race).
func TestParseIndexDefConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				def := fmt.Sprintf("CREATE INDEX c%d ON s.t USING btree (k%d)", i%50, i%50)
				if p := ParseIndexDef(def); len(p.Columns) != 1 ||
					p.Columns[0] != fmt.Sprintf("k%d", i%50) {
					t.Errorf("goroutine %d: %s -> %+v", g, def, p)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
