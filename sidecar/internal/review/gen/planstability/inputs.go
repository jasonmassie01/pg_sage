package planstability

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// inputs are the pack sections the generator reads.
type inputs struct {
	workload workpack.Workload
	texts    map[string]string            // queryid -> normalized text
	statIDs  map[string]string            // "schema.table.column" -> stats fact id
	plans    map[string]workpack.PlanFact // queryid -> latest plan
}

// parsedQuery is one top query and the filters its text names.
type parsedQuery struct {
	q     workpack.Query
	rels  []string
	preds []predicate
}

// readPack decodes the workload, texts, stats and plans sections. A pack
// without texts has no predicates to read: "texts" is missing.
func readPack(pack *workpack.Pack, cov *candidate.Coverage) (inputs, error) {
	in := inputs{texts: map[string]string{}, statIDs: map[string]string{},
		plans: map[string]workpack.PlanFact{}}
	if err := json.Unmarshal(pack.File("workload.json"), &in.workload); err != nil {
		return in, fmt.Errorf("decode workload.json: %w", err)
	}
	var texts workpack.Texts
	if err := decodeOptional(pack, "texts.json", &texts, cov, "texts"); err != nil {
		return in, err
	}
	for _, t := range texts.Texts {
		in.texts[t.QueryID] = t.Text
	}
	var stats workpack.Stats
	if err := decodeOptional(pack, "stats.json", &stats, cov, ""); err != nil {
		return in, err
	}
	for _, c := range stats.Columns {
		in.statIDs[c.Table+"."+c.Column] = c.ID
	}
	var plans workpack.Plans
	if err := decodeOptional(pack, "plans.json", &plans, cov, ""); err != nil {
		return in, err
	}
	for _, p := range plans.Plans {
		in.plans[p.QueryID] = p
	}
	return in, nil
}

// decodeOptional decodes a section that may be absent (missing names it
// in the coverage when it is needed).
func decodeOptional(pack *workpack.Pack, file string, dst any, cov *candidate.Coverage,
	missing string) error {
	raw := pack.File(file)
	if raw == nil {
		if missing != "" {
			cov.AddMissing(missing)
		}
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode %s: %w", file, err)
	}
	return nil
}

// parse reads the top queries' filters, at most MaxTopQueries by total
// time; a query without text, or whose text does not lex, is skipped
// with the reason.
func (in inputs) parse(cov *candidate.Coverage) []parsedQuery {
	var all []parsedQuery
	for _, fam := range in.workload.Families {
		for _, q := range fam.QueryIDs {
			all = append(all, parsedQuery{q: q, rels: fam.Relations})
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].q.ExecMs != all[b].q.ExecMs {
			return all[a].q.ExecMs > all[b].q.ExecMs
		}
		return all[a].q.QueryID < all[b].q.QueryID
	})
	if len(all) > MaxTopQueries {
		cov.Note("%d queries listed; the top %d by total time were read", len(all),
			MaxTopQueries)
		all = all[:MaxTopQueries]
	}
	for i := range all {
		text, ok := in.texts[all[i].q.QueryID]
		if !ok {
			cov.Note("query %s skipped: no statement text in the pack", all[i].q.QueryID)
			continue
		}
		preds, err := predicates(text)
		if err != nil {
			cov.Note("query %s skipped: %v", all[i].q.QueryID, err)
			continue
		}
		all[i].preds = preds
	}
	return all
}
