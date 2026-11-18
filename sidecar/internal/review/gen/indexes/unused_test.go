package indexes

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

func unusedFact(name string) workpack.IndexFact {
	f := fact("v", name, "z", 4096)
	f.Scans = int64(0)
	return f
}

func TestGenerate_ZeroScansOverTheWholeWindowIsUnused(t *testing.T) {
	pack := mustPack(t, spec("db1", 90, "ok", unusedFact("v_z")))
	cands, _, err := New(nil).Generate(context.Background(), pack, env("db1"))
	if err != nil || len(cands) != 1 {
		t.Fatalf("%d candidates (%+v), err %v; want 1", len(cands), cands, err)
	}
	c := cands[0]
	if c.Kind != KindUnused || c.ActionType != "drop_unused_index" ||
		c.Target != "public.v_z" || c.Prediction.Tier != candidate.TierNone ||
		!strings.Contains(c.Reason, "90 days") {
		t.Fatalf("unused step %+v", c)
	}
	if !slices.Equal(c.EvidenceIDs, []string{"index/idx/public.v_z"}) ||
		c.Validate(pack) != nil {
		t.Fatalf("evidence %v, validate %v", c.EvidenceIDs, c.Validate(pack))
	}
}

func TestGenerate_AnEpochBreakMakesZeroScansUnknown(t *testing.T) {
	// workpack reports a statistics reset inside the window as "reset" and
	// every index's scans as unknown, whatever the counters said.
	f := unusedFact("v_z")
	f.Scans = "unknown"
	pack := mustPack(t, spec("db1", 90, "reset", f))
	cands, cov, err := New(nil).Generate(context.Background(), pack, env("db1"))
	if err != nil || len(cands) != 0 {
		t.Fatalf("%d candidates (%+v), err %v; want none", len(cands), cands, err)
	}
	if !strings.Contains(strings.Join(cov.Notes, " "), "unknown") {
		t.Fatalf("notes %v do not say the scans are unknown", cov.Notes)
	}
	// Even a zero under a reset status is not believed.
	f.Scans = int64(0)
	pack = mustPack(t, spec("db1", 90, "reset", f))
	if cands, _, _ := New(nil).Generate(context.Background(), pack, env("db1")); len(cands) != 0 {
		t.Fatalf("zero scans across a reset gave %+v", cands)
	}
}

func TestGenerate_UndroppableIndexesAreNeverProposed(t *testing.T) {
	mods := map[string]func(*workpack.IndexFact){
		"unique":     func(f *workpack.IndexFact) { f.Unique = true },
		"primary":    func(f *workpack.IndexFact) { f.Primary = true },
		"constraint": func(f *workpack.IndexFact) { f.ConstraintBacking = true },
		"invalid":    func(f *workpack.IndexFact) { f.Valid = false },
		"used":       func(f *workpack.IndexFact) { f.Scans = int64(1) },
	}
	for name, mod := range mods {
		f := unusedFact("v_z")
		mod(&f)
		pack := mustPack(t, spec("db1", 90, "ok", f))
		cands, _, err := New(nil).Generate(context.Background(), pack, env("db1"))
		if err != nil || len(cands) != 0 {
			t.Errorf("%s: %d candidates, err %v; want none", name, len(cands), err)
		}
	}
}

func TestGenerate_PartitionIndexesAreSkipped(t *testing.T) {
	// schema.json folds partitions into their root: an index whose table is
	// not listed there belongs to a partition and cannot be dropped alone.
	s := spec("db1", 90, "ok", unusedFact("v_z"))
	s.Schema = &workpack.Schema{Tables: []workpack.TableFact{{ID: "schema/table/public.t",
		Table: "public.t", Columns: []workpack.ColumnFact{}, Constraints: []string{},
		Indexes: []string{}}}}
	cands, _, err := New(nil).Generate(context.Background(), mustPack(t, s), env("db1"))
	if err != nil || len(cands) != 0 {
		t.Fatalf("%d candidates (%+v), err %v; want none", len(cands), cands, err)
	}
}

func TestGenerate_NilPackIsAnError(t *testing.T) {
	_, _, err := New(nil).Generate(context.Background(), nil, env("db1"))
	if err == nil || !strings.Contains(err.Error(), "pack") {
		t.Fatalf("nil pack: err = %v", err)
	}
}

func TestGenerate_NoIndexSectionGivesNothingAndSaysSo(t *testing.T) {
	s := spec("db1", 90, "ok")
	s.Indexes = nil
	cands, cov, err := New(nil).Generate(context.Background(), mustPack(t, s), env("db1"))
	if err != nil || len(cands) != 0 {
		t.Fatalf("%d candidates, err %v", len(cands), err)
	}
	if !slices.Equal(cov.Missing, []string{"indexes"}) || cov.Status != candidate.CoveragePartial {
		t.Fatalf("coverage %+v, want partial with indexes missing", cov)
	}
}

func TestGenerate_EmptyIndexSectionGivesNothing(t *testing.T) {
	cands, cov, err := New(nil).Generate(context.Background(),
		mustPack(t, spec("db1", 90, "ok")), env("db1"))
	if err != nil || len(cands) != 0 || cov.Status != candidate.CoverageComplete {
		t.Fatalf("%d candidates, coverage %+v, err %v", len(cands), cov, err)
	}
}

func TestGenerate_AWindowADayShortIsPartial(t *testing.T) {
	pack := mustPack(t, spec("db1", 89, "ok", unusedFact("v_z")))
	cands, cov, err := New(nil).Generate(context.Background(), pack, env("db1"))
	if err != nil || cov.Status != candidate.CoveragePartial || cov.WindowDays != 89 {
		t.Fatalf("coverage %+v, err %v; want partial over 89 days", cov, err)
	}
	if len(cands) != 1 || !strings.Contains(cands[0].Reason, "89 days") {
		t.Fatalf("candidates %+v, want the unused step over 89 days", cands)
	}
}

func TestGenerate_InvalidEnvIsRefused(t *testing.T) {
	pack := mustPack(t, spec("db1", 90, "ok", unusedFact("v_z")))
	for _, e := range []candidate.Env{{Database: "db1", WindowDays: 0},
		{Database: "db1", WindowDays: 90, MinImprovementPct: -1},
		{Database: "", WindowDays: 90}} {
		if _, _, err := New(nil).Generate(context.Background(), pack, e); err == nil {
			t.Errorf("env %+v accepted", e)
		}
	}
}

func TestGenerate_CapsPerTable(t *testing.T) {
	facts := []workpack.IndexFact{
		fact("t", "t_a", "a", 10, "public.t_ab"), fact("t", "t_ab", "a, b", 20),
		fact("t", "t_x", "x", 30, "public.t_xy"), fact("t", "t_xy", "x, y", 40),
		fact("t", "t_y", "y", 50, "public.t_yz", "public.t_yzw"),
		fact("t", "t_yz", "y, z", 60, "public.t_yzw"), fact("t", "t_yzw", "y, z, w", 70),
	}
	pack := mustPack(t, spec("db1", 90, "ok", facts...))
	m := measured()
	cands, cov, err := New(m).Generate(context.Background(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	// One set per table: the largest (y, y z under y z w).
	if got := targets(cands); !slices.Equal(got, []string{"public.t_y", "public.t_yz"}) {
		t.Fatalf("targets %v, want the y set only", got)
	}
	if len(m.sets) != 1 || !strings.Contains(strings.Join(cov.Notes, " "), "one set") {
		t.Fatalf("%d sets measured, notes %v", len(m.sets), cov.Notes)
	}
	e := env("db1")
	e.MaxIndexesPerTable = 1
	cands, _, _ = New(measured()).Generate(context.Background(), pack, e)
	if len(cands) != 1 {
		t.Fatalf("max_indexes_per_table 1 gave %d steps", len(cands))
	}
}

func targets(cands []candidate.Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Target)
	}
	slices.Sort(out)
	return out
}

// Concurrent access: generators keep no state between calls, so two
// databases run side by side, and one pack twice gives one set of ids.
func TestGenerate_ParallelDatabasesShareNoState(t *testing.T) {
	for _, db := range []string{"db1", "db2"} {
		t.Run(db, func(t *testing.T) {
			t.Parallel()
			facts := chainFacts()
			if db == "db2" {
				facts = []workpack.IndexFact{unusedFact("v_z")}
			}
			pack := mustPack(t, spec(db, 90, "ok", facts...))
			cands, _, err := New(measured()).Generate(context.Background(), pack, env(db))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int{"db1": 2, "db2": 1}[db]
			if len(cands) != want {
				t.Fatalf("%s: %d candidates, want %d", db, len(cands), want)
			}
		})
	}
}

func TestGenerate_SamePackTwiceInParallelGivesTheSameIDs(t *testing.T) {
	facts := append(chainFacts(), unusedFact("v_z"))
	pack := mustPack(t, spec("db1", 90, "ok", facts...))
	g := New(measured())
	var wg sync.WaitGroup
	ids := make([][]string, 2)
	errs := make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cands, _, err := g.Generate(context.Background(), pack, env("db1"))
			errs[i] = err
			for _, c := range cands {
				ids[i] = append(ids[i], c.ID)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if len(ids[0]) != 3 || !slices.Equal(ids[0], ids[1]) {
		t.Fatalf("ids %v and %v, want the same 3", ids[0], ids[1])
	}
}

func TestGenerate_IDsAreStableAcrossRebuiltPacks(t *testing.T) {
	first, _, _ := New(measured()).Generate(context.Background(),
		mustPack(t, spec("db1", 90, "ok", chainFacts()...)), env("db1"))
	second, _, _ := New(measured()).Generate(context.Background(),
		mustPack(t, spec("db1", 90, "ok", chainFacts()...)), env("db1"))
	if len(first) != 2 || len(second) != 2 || first[0].ID != second[0].ID ||
		first[1].ID != second[1].ID {
		t.Fatalf("ids changed between two runs: %v / %v", first, second)
	}
}
