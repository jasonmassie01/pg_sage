package indexes

import (
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// portfolio is the pack's index section, keyed for the generator.
type portfolio struct {
	pack   *workpack.Pack
	facts  []workpack.IndexFact // the section's order: schema, then index name
	byName map[string]workpack.IndexFact
	// tables are the schema section's tables and partition roots by
	// schema.table; nil when the pack has no schema section.
	tables    map[string]bool
	perTable  map[string]int // indexes per schema.table
	status    string         // indexes.json scans_status
	truncated bool
}

func qualified(schema, name string) string { return schema + "." + name }

func factName(f workpack.IndexFact) string  { return qualified(f.Schema, f.Index) }
func factTable(f workpack.IndexFact) string { return qualified(f.Schema, f.Table) }

// readPortfolio decodes the index section (ok false when the pack has
// none) and the schema section's tables when present.
func readPortfolio(pack *workpack.Pack) (portfolio, bool, error) {
	raw := pack.File("indexes.json")
	if raw == nil {
		return portfolio{}, false, nil
	}
	var sec workpack.Indexes
	if err := json.Unmarshal(raw, &sec); err != nil {
		return portfolio{}, false, fmt.Errorf("decode indexes.json: %w", err)
	}
	pf := portfolio{pack: pack, facts: sec.Indexes, status: sec.ScansStatus,
		truncated: sec.Truncated, byName: map[string]workpack.IndexFact{},
		perTable: map[string]int{}}
	for _, f := range sec.Indexes {
		pf.byName[factName(f)] = f
		pf.perTable[factTable(f)]++
	}
	if raw := pack.File("schema.json"); raw != nil {
		var sch workpack.Schema
		if err := json.Unmarshal(raw, &sch); err != nil {
			return portfolio{}, false, fmt.Errorf("decode schema.json: %w", err)
		}
		pf.tables = map[string]bool{}
		for _, t := range sch.Tables {
			pf.tables[t.Table] = true
		}
	}
	return pf, true, nil
}

// droppable reports an index that may be dropped on its own: valid, not
// unique, primary or constraint-backing (the analyzer's
// isConstraintBacked), and not a partition's (isUndroppable). The schema
// section folds partitions into their root, so an index of a table it
// does not list is a partition's.
func (pf portfolio) droppable(f workpack.IndexFact) bool {
	if !f.Valid || f.Unique || f.Primary || f.ConstraintBacking {
		return false
	}
	return pf.tables == nil || pf.tables[factTable(f)]
}

// plainBtree is an index a consolidation set may hold: btree, no predicate.
func plainBtree(f workpack.IndexFact) bool {
	return f.Valid && f.Method == "btree" && !f.Partial
}

// scans is the window's scan count; ok false when it is not a number.
func scans(f workpack.IndexFact) (float64, bool) {
	n, ok := f.Scans.(float64)
	return n, ok
}

// writesPerDay is the table's inserted and updated rows a day over the
// window; ok false when the writes are unknown.
func writesPerDay(f workpack.IndexFact, days int) (float64, bool) {
	m, ok := f.TableWrites.(map[string]any)
	if !ok || days < 1 {
		return 0, false
	}
	ins, okI := m["inserts"].(float64)
	upd, okU := m["updates"].(float64)
	if !okI || !okU {
		return 0, false
	}
	return (ins + upd) / float64(days), true
}
