package analyzer

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// Found on the lifeos dogfood (35k indexes, 15k tables, 160 clone
// schemas): the analyzer pinned 1-2 cores of the sidecar every cycle in
// ruleDuplicateIndexes, which compared every btree index with every other
// one in the database (~600M pairs, two string builds each), and the
// unused-index rule re-parsed every index definition for each unused
// index backing a foreign key. Duplicates, subsets and FK support only
// exist within one table, so both now work per table.

// scaleFixture builds schemas x tables with the shapes lifeos has: clone
// schemas reusing table names, subsets, duplicates, partial and INCLUDE
// indexes, and unused indexes that back (or share backing of) an FK.
func scaleFixture(schemas, tables int) *collector.Snapshot {
	snap := &collector.Snapshot{}
	for s := 0; s < schemas; s++ {
		schema := fmt.Sprintf("test_clone_%03d", s)
		for n := 0; n < tables; n++ {
			table := fmt.Sprintf("t_%04d", n)
			snap.Tables = append(snap.Tables, collector.TableStats{
				SchemaName: schema, RelName: table})
			snap.Indexes = append(snap.Indexes, tableIndexes(schema, table, n)...)
			if s == 0 && n%3 == 0 {
				snap.ForeignKeys = append(snap.ForeignKeys, collector.ForeignKey{
					TableName: table, ReferencedTable: "parent", FKColumn: "fk_id",
					ConstraintName: table + "_fk_id_fkey"})
			}
		}
	}
	return snap
}

func tableIndexes(schema, table string, n int) []collector.IndexStats {
	mk := func(name, cols, extra string, unique, primary bool) collector.IndexStats {
		kind := "INDEX"
		if unique {
			kind = "UNIQUE INDEX"
		}
		return collector.IndexStats{SchemaName: schema, RelName: table,
			IndexRelName: name, IsValid: true, IsUnique: unique, IsPrimary: primary,
			IndexType: "btree", IndexBytes: int64(8192 * (n%7 + 1)),
			IndexDef: fmt.Sprintf("CREATE %s %s ON %s.%s USING btree (%s)%s",
				kind, name, schema, table, cols, extra)}
	}
	out := []collector.IndexStats{
		mk(table+"_pkey", "id", "", true, true),
		mk(table+"_a", "a", "", false, false),
		mk(table+"_ab", "a, b", "", false, false),
	}
	if n%7 == 0 {
		out = append(out, mk(table+"_a_dup", "a", "", false, false))
	}
	if n%5 == 0 {
		out = append(out, mk(table+"_c_pos", "c", " WHERE (c > 0)", false, false),
			mk(table+"_c", "c", "", false, false))
	}
	if n%3 == 0 {
		out = append(out, mk(table+"_fk", "fk_id", "", false, false))
		if n%6 == 0 {
			out = append(out, mk(table+"_fk_x", "fk_id, x", "", false, false))
		}
	}
	if n%11 == 0 {
		out = append(out, mk(table+"_d_inc", "d", " INCLUDE (e)", false, false),
			mk(table+"_de", "d, e", "", false, false))
	}
	return out
}

// duplicateIndexesReference is the previous all-pairs algorithm, kept as
// the oracle the per-table version must match.
func duplicateIndexesReference(current *collector.Snapshot) []Finding {
	var btrees []duplicateIndexCandidate
	for _, idx := range current.Indexes {
		if isSystemSchema(idx.SchemaName) || !idx.IsValid {
			continue
		}
		p := ParseIndexDef(idx.IndexDef)
		if p.IndexType != "btree" {
			continue
		}
		btrees = append(btrees, duplicateIndexCandidate{info: idx, parsed: p})
	}
	seen := map[string]bool{}
	var out []Finding
	for i := 0; i < len(btrees); i++ {
		for j := i + 1; j < len(btrees); j++ {
			out = appendPairFinding(out, seen, btrees[i], btrees[j])
		}
	}
	return out
}

// onlyFKSupportReference is the previous whole-database scan.
func onlyFKSupportReference(idx collector.IndexStats, all []collector.IndexStats,
	requirements map[tableKey][][]string) bool {
	if !indexSupportsFKRequirement(idx, requirements) {
		return false
	}
	p := ParseIndexDef(idx.IndexDef)
	schema := p.Schema
	if schema == "" {
		schema = idx.SchemaName
	}
	for _, req := range requirements[tableKey{schema, p.Table}] {
		if !isLeadingSet(req, p.Columns) {
			continue
		}
		for _, other := range all {
			if other.IndexRelName == idx.IndexRelName && other.SchemaName == idx.SchemaName {
				continue
			}
			if indexCoversRequirement(other, req, schema, p.Table) {
				return false
			}
		}
		return true
	}
	return false
}

func findingKeys(fs []Finding) []string {
	keys := make([]string, 0, len(fs))
	for _, f := range fs {
		keys = append(keys, fmt.Sprintf("%s|%s|%v|%v|%s", f.Category, f.ObjectIdentifier,
			f.Detail["drop_index"], f.Detail["keep_index"], f.RecommendedSQL))
	}
	sort.Strings(keys)
	return keys
}

func TestDuplicateIndexes_PerTableMatchesAllPairs(t *testing.T) {
	snap := scaleFixture(4, 60)
	want := findingKeys(duplicateIndexesReference(snap))
	got := findingKeys(ruleDuplicateIndexes(snap, nil, nil, nil))
	if len(want) == 0 {
		t.Fatal("fixture produced no duplicate/subset findings: it tests nothing")
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("per-table findings differ from all-pairs:\n got %d %v\nwant %d %v",
			len(got), got, len(want), want)
	}
}

func TestOnlyFKSupport_TableIndexMatchesWholeScan(t *testing.T) {
	snap := scaleFixture(4, 60)
	reqs := buildFKRequirements(snap)
	byTable := indexesByTable(snap.Indexes)
	var sole, shared int
	for _, idx := range snap.Indexes {
		want := onlyFKSupportReference(idx, snap.Indexes, reqs)
		if got := indexIsOnlyFKSupport(idx, byTable, reqs); got != want {
			t.Fatalf("%s.%s: only FK support = %t, want %t", idx.SchemaName,
				idx.IndexRelName, got, want)
		}
		if want {
			sole++
		} else if indexSupportsFKRequirement(idx, reqs) {
			shared++
		}
	}
	if sole == 0 || shared == 0 {
		t.Fatalf("fixture must cover sole (%d) and shared (%d) FK support", sole, shared)
	}
}

// At lifeos scale (~42k indexes) both rules finish within a budget that
// the all-pairs version missed by far: it took 92.7 s on this fixture,
// the per-table version 0.19 s (4.4 s under the race detector, as CI runs
// it).
func TestIndexRules_LinearAtLifeosScale(t *testing.T) {
	snap := scaleFixture(40, 250)
	if n := len(snap.Indexes); n < 35000 {
		t.Fatalf("fixture has %d indexes, want lifeos scale", n)
	}
	cfg := config.DefaultConfig()
	extras := defaultExtras()
	started := time.Now()
	dups := ruleDuplicateIndexes(snap, nil, cfg, extras)
	unused := ruleUnusedIndexes(snap, nil, cfg, extras)
	elapsed := time.Since(started)
	if elapsed > 20*time.Second {
		t.Fatalf("duplicate+unused index rules took %s on %d indexes", elapsed,
			len(snap.Indexes))
	}
	if len(dups) == 0 {
		t.Fatal("no duplicate findings at scale: the rule did not run")
	}
	t.Logf("%d indexes: %d duplicate/subset, %d unused findings in %s",
		len(snap.Indexes), len(dups), len(unused), elapsed)
}
