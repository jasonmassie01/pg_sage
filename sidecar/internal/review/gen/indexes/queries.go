package indexes

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/workload"
)

// maxSetQueries bounds the statements a set is planned over; the
// optimizer samples at most its own maxWhatIfStatements of them.
const maxSetQueries = 50

// tableQueries are the pack's top queryids that read or write
// schema.table (workload.ReferencedTables over their texts, with the
// pack's indexed tables as the known relations), hottest first.
func tableQueries(pack *workpack.Pack, schema, table string) []optimizer.QueryInfo {
	var wl workpack.Workload
	var texts workpack.Texts
	if json.Unmarshal(pack.File("workload.json"), &wl) != nil ||
		json.Unmarshal(pack.File("texts.json"), &texts) != nil {
		return nil
	}
	known := knownTables(pack)
	byID := map[string]string{}
	for _, t := range texts.Texts {
		byID[t.QueryID] = t.Text
	}
	var out []optimizer.QueryInfo
	for _, fam := range wl.Families {
		for _, q := range fam.QueryIDs {
			text, ok := byID[q.QueryID]
			id, err := strconv.ParseInt(q.QueryID, 10, 64)
			if !ok || err != nil || !references(text, known, schema, table) {
				continue
			}
			info := optimizer.QueryInfo{QueryID: id, Text: text, Calls: q.Calls,
				TotalTimeMs: q.ExecMs}
			if q.Calls > 0 {
				info.MeanTimeMs = q.ExecMs / float64(q.Calls)
			}
			out = append(out, info)
		}
	}
	slices.SortStableFunc(out, func(a, b optimizer.QueryInfo) int {
		return cmp.Or(cmp.Compare(b.TotalTimeMs, a.TotalTimeMs), cmp.Compare(a.QueryID,
			b.QueryID))
	})
	return out[:min(len(out), maxSetQueries)]
}

// knownTables maps each indexed table's name to its schemas.
func knownTables(pack *workpack.Pack) map[string][]string {
	var sec workpack.Indexes
	known := map[string][]string{}
	if json.Unmarshal(pack.File("indexes.json"), &sec) != nil {
		return known
	}
	for _, f := range sec.Indexes {
		if !slices.Contains(known[f.Table], f.Schema) {
			known[f.Table] = append(known[f.Table], f.Schema)
		}
	}
	return known
}

func references(text string, known map[string][]string, schema, table string) bool {
	for _, r := range workload.ReferencedTables(text, known) {
		if r.Schema == schema && r.Name == table {
			return true
		}
	}
	return false
}
