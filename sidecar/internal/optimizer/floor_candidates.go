package optimizer

import (
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// floorMaxSelectivity is the largest equality selectivity (the fraction
// of rows one value matches: its most common value's frequency, else one
// over the distinct count) a floor key column may have. A column whose
// statistics are unknown never qualifies.
const floorMaxSelectivity = 0.20

// floorMaxEqualityKeys bounds the equality keys of the composite candidate.
const floorMaxEqualityKeys = 2

// floorCand is one candidate's keys and the calls of the statements that
// filter on them.
type floorCand struct {
	keys  []string
	calls int64
}

// floorWorkload is a table's parsed statements with the calls filtering
// on each equality and range column.
type floorWorkload struct {
	stmts    []floorFilter
	eqCalls  map[string]int64
	rngCalls map[string]int64
}

func newFloorWorkload(tc TableContext) floorWorkload {
	w := floorWorkload{eqCalls: map[string]int64{}, rngCalls: map[string]int64{}}
	for _, q := range tc.Queries {
		f, ok := parseFloorFilters(q.Text, tc.Table)
		if !ok {
			continue
		}
		f.calls = q.Calls
		w.stmts = append(w.stmts, f)
		for _, c := range f.eq {
			w.eqCalls[c] += q.Calls
		}
		for _, c := range f.rng {
			w.rngCalls[c] += q.Calls
		}
	}
	return w
}

// floorCandidates are a table's deterministic candidates, most-called
// first, at most floorMaxPerTable: a single-column btree on each
// selective equality column, and one composite (equality columns first,
// then one range column) that replaces the single index on its leading
// column. A candidate an existing valid index already covers is dropped.
func floorCandidates(tc TableContext) []Recommendation {
	if tc.Table == "" || len(tc.Queries) == 0 {
		return nil
	}
	w := newFloorWorkload(tc)
	cands := floorSingles(tc, w)
	if c, ok := floorComposite(tc, w); ok {
		cands = slices.DeleteFunc(cands, func(s floorCand) bool { return s.keys[0] == c.keys[0] })
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].calls != cands[j].calls {
			return cands[i].calls > cands[j].calls
		}
		return strings.Join(cands[i].keys, ",") < strings.Join(cands[j].keys, ",")
	})
	var recs []Recommendation
	for _, c := range cands {
		rec := floorRecommendation(tc, c)
		if floorCovered(tc, rec.DDL) {
			continue
		}
		if recs = append(recs, rec); len(recs) == floorMaxPerTable {
			break
		}
	}
	return recs
}

func floorSingles(tc TableContext, w floorWorkload) []floorCand {
	var out []floorCand
	for col, calls := range w.eqCalls {
		if sel, ok := floorSelectivity(tc, col); ok && sel <= floorMaxSelectivity &&
			floorColumnExists(tc, col) {
			out = append(out, floorCand{keys: []string{col}, calls: calls})
		}
	}
	return out
}

// floorComposite is the most-called composite: a statement's known
// equality columns (most-called first, at most floorMaxEqualityKeys, of
// combined selectivity within the cutoff) and then one of its range
// columns. A statement needs at least two such keys.
func floorComposite(tc TableContext, w floorWorkload) (floorCand, bool) {
	byKeys := map[string]*floorCand{}
	for _, s := range w.stmts {
		keys, ok := floorCompositeKeys(tc, w, s)
		if !ok {
			continue
		}
		id := strings.Join(keys, ",")
		if byKeys[id] == nil {
			byKeys[id] = &floorCand{keys: keys}
		}
		byKeys[id].calls += s.calls
	}
	var best *floorCand
	for _, c := range byKeys {
		if best == nil || c.calls > best.calls || (c.calls == best.calls &&
			strings.Join(c.keys, ",") < strings.Join(best.keys, ",")) {
			best = c
		}
	}
	if best == nil {
		return floorCand{}, false
	}
	return *best, true
}

func floorCompositeKeys(tc TableContext, w floorWorkload, s floorFilter) ([]string, bool) {
	var eq []string
	for _, c := range s.eq {
		if _, ok := floorSelectivity(tc, c); ok && floorColumnExists(tc, c) {
			eq = append(eq, c)
		}
	}
	byCalls(eq, w.eqCalls)
	eq = eq[:min(len(eq), floorMaxEqualityKeys)]
	product := 1.0
	for _, c := range eq {
		sel, _ := floorSelectivity(tc, c)
		product *= sel
	}
	if len(eq) == 0 || product > floorMaxSelectivity {
		return nil, false
	}
	rng := slices.DeleteFunc(slices.Clone(s.rng), func(c string) bool {
		return !floorColumnExists(tc, c)
	})
	byCalls(rng, w.rngCalls)
	keys := append(eq, rng[:min(len(rng), 1)]...)
	return keys, len(keys) >= 2
}

// byCalls orders cols by their calls, most first, then by name.
func byCalls(cols []string, calls map[string]int64) {
	sort.SliceStable(cols, func(i, j int) bool {
		if calls[cols[i]] != calls[cols[j]] {
			return calls[cols[i]] > calls[cols[j]]
		}
		return cols[i] < cols[j]
	})
}

// floorSelectivity is the fraction of rows one value of col (its exact
// name, as the parser folded it) matches: the most common value's
// frequency, else one over the distinct count. ok is false without
// statistics for the column.
func floorSelectivity(tc TableContext, col string) (float64, bool) {
	for _, cs := range tc.ColStats {
		if cs.Column != col {
			continue
		}
		if len(cs.MostCommonFreqs) > 0 {
			return cs.MostCommonFreqs[0], true
		}
		distinct := cs.NDistinct
		if distinct < 0 {
			distinct = -distinct * float64(tc.LiveTuples)
		}
		if distinct >= 1 {
			return 1 / distinct, true
		}
		return 1, false
	}
	return 1, false
}

// floorColumnExists: the column is in this cycle's column list, or, when
// the list is unknown, has statistics on the table.
func floorColumnExists(tc TableContext, col string) bool {
	if tc.ColumnsKnown {
		return slices.ContainsFunc(tc.Columns, func(c ColumnInfo) bool {
			return c.Name == col
		})
	}
	_, ok := floorSelectivity(tc, col)
	return ok
}

func floorCovered(tc TableContext, ddl string) bool {
	for _, ix := range tc.Indexes {
		if ix.IsValid && CoveredBy(ddl, ix.Definition) {
			return true
		}
	}
	return false
}

// floorRecommendation is the candidate as a recommendation for Admit.
func floorRecommendation(tc TableContext, c floorCand) Recommendation {
	quoted := make([]string, len(c.keys))
	for i, k := range c.keys {
		quoted[i] = sqlident.Ident(k)
	}
	ddl := fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s (%s)",
		sqlident.Ident(floorIndexName(tc.Table, c.keys)), sqlident.Qualified(tc.Schema, tc.Table),
		strings.Join(quoted, ", "))
	return Recommendation{Table: tc.Schema + "." + tc.Table, DDL: ddl, Severity: "info",
		IndexType: "btree", Category: OptimizerCategory,
		Rationale: fmt.Sprintf("Deterministic index floor (no tuning agent this cycle): "+
			"statements making %d calls filter %s.%s on %s, and the table is read by "+
			"sequential scans.", c.calls, tc.Schema, tc.Table, strings.Join(c.keys, ", "))}
}

// floorIndexName is sage_<table>_<keys>_idx, lower-case, within
// PostgreSQL's 63-byte limit (a long name keeps a hash of the full one).
func floorIndexName(table string, keys []string) string {
	raw := "sage_" + table + "_" + strings.Join(keys, "_") + "_idx"
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '_'
	}, raw)
	if len(clean) <= 63 {
		return clean
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	return fmt.Sprintf("%s_%08x", clean[:54], h.Sum32())
}
