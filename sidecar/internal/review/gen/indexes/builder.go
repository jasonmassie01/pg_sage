package indexes

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// minUnusedWindowDays is the shortest window over which no scan makes an
// index unused: review.window_days' minimum. A younger pack (a new install,
// short retention) calls no index unused.
const minUnusedWindowDays = 14

// writeCostSource says how the write cost is computed.
const writeCostSource = "indexes.json table_writes over the pack window: one leaf block " +
	"per inserted or updated row per index (an upper bound; a HOT update writes none)"

// builder collects one Generate call's candidates. It lives for one call,
// so generators share no state.
type builder struct {
	pf    portfolio
	env   candidate.Env
	cov   *candidate.Coverage
	days  int
	taken map[string]bool // indexes a candidate already targets
	// kept are the indexes a proposed drop moves lookups onto (a set's
	// cover, a duplicate's kept copy): no candidate of the review drops one.
	kept     map[string]bool
	perTable map[string]int  // candidates per schema.table
	capped   map[string]bool // tables whose cap a candidate hit
	out      []candidate.Candidate
}

func newBuilder(pf portfolio, env candidate.Env, cov *candidate.Coverage) *builder {
	if pf.truncated {
		cov.Status = candidate.CoveragePartial
		cov.Note("the pack lists only the first %d indexes", len(pf.facts))
	}
	return &builder{pf: pf, env: env, cov: cov, days: cov.WindowDays,
		taken: map[string]bool{}, kept: map[string]bool{}, perTable: map[string]int{},
		capped: map[string]bool{}}
}

// room reports whether the table may take another candidate besides the
// pending ones not yet added (optimizer.max_indexes_per_table; 0 is no
// cap), noting a hit cap once.
func (b *builder) room(table string, pending int) bool {
	if b.env.MaxIndexesPerTable == 0 ||
		b.perTable[table]+pending < b.env.MaxIndexesPerTable {
		return true
	}
	if !b.capped[table] {
		b.capped[table] = true
		b.cov.Note("%s: optimizer.max_indexes_per_table (%d) reached; the rest waits for "+
			"the next review", table, b.env.MaxIndexesPerTable)
	}
	return false
}

// drop is the step dropping f, citing f's fact and then the others'.
func (b *builder) drop(f workpack.IndexFact, kind, reason, tier string,
	others ...workpack.IndexFact,
) candidate.Candidate {
	c := candidate.Candidate{Generator: Name, Dimension: Dimension, Kind: kind,
		Target: factName(f), ActionType: "drop_unused_index",
		ForwardSQL:  analyzer.DropIndexSQL(f.Schema, f.Index),
		RollbackSQL: f.Definition + ";", EvidenceIDs: []string{f.ID}, Reversible: true,
		Reason: reason, Approval: candidate.ApprovalAutomatic,
		Prediction: b.predict(f, tier)}
	for _, o := range others {
		c.EvidenceIDs = append(c.EvidenceIDs, o.ID)
	}
	c.ID = candidate.NewID(c.Generator, c.Kind, c.Target, c.ForwardSQL)
	return c
}

// advise turns a step into advice: never run, its effect unknown.
func advise(c candidate.Candidate, why string) candidate.Candidate {
	c.ActionType, c.Approval = candidate.ActionAdvice, candidate.ApprovalRequired
	c.Reason = c.Reason + "; advice only: " + why
	c.Prediction.Direction, c.Prediction.Tier = "unknown", candidate.TierNone
	c.Prediction.SizePct = nil
	return c
}

// add keeps c and marks its target taken.
func (b *builder) add(c candidate.Candidate, table string) {
	b.taken[c.Target] = true
	b.perTable[table]++
	b.out = append(b.out, c)
}

// predict is the drop's write cost and bytes from the pack's facts.
func (b *builder) predict(f workpack.IndexFact, tier string) candidate.Prediction {
	p := candidate.Prediction{Metric: MetricIndexBytes, Direction: "decrease", Tier: tier,
		Source:     "indexes.json size_bytes",
		Quantities: map[string]float64{QuantityBytes: float64(f.SizeBytes)}}
	perDay, ok := writesPerDay(f, b.days)
	if !ok {
		return p
	}
	share := 100 / float64(max(b.pf.perTable[factTable(f)], 1))
	p.Metric, p.Source, p.SizePct = MetricWriteBlocks, writeCostSource, &share
	p.Quantities[QuantityWriteBlocks] = perDay
	return p
}

// duplicates drops every exact copy but the one to keep.
func (b *builder) duplicates() {
	seen := map[string]bool{}
	for _, f := range b.pf.facts {
		if len(f.DuplicateOf) == 0 || seen[factName(f)] {
			continue
		}
		group := []workpack.IndexFact{f}
		for _, name := range f.DuplicateOf {
			if o, ok := b.pf.byName[name]; ok {
				group = append(group, o)
			}
		}
		slices.SortFunc(group, keepFirst)
		for _, g := range group {
			seen[factName(g)] = true
		}
		keep := group[0]
		for _, d := range group[1:] {
			if !b.pf.droppable(d) || b.taken[factName(d)] || !b.room(factTable(d), 0) {
				continue
			}
			b.kept[factName(keep)] = true
			b.add(b.drop(d, KindDuplicate, fmt.Sprintf("identical to %s (same keys, "+
				"operator classes, collations and predicate): every write maintains both",
				factName(keep)), candidate.TierNone, keep), factTable(d))
		}
	}
}

// keepFirst orders copies of one index: the one to keep first, then the
// one the workload scanned most.
func keepFirst(a, b workpack.IndexFact) int {
	rank := func(f workpack.IndexFact) int {
		switch {
		case f.ConstraintBacking || f.Primary:
			return 0
		case f.Unique:
			return 1
		case f.Valid:
			return 2
		}
		return 3
	}
	scanned := func(f workpack.IndexFact) float64 {
		if n, ok := scans(f); ok {
			return n
		}
		return -1
	}
	return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(scanned(b), scanned(a)),
		cmp.Compare(factName(a), factName(b)))
}

// unused drops indexes with no scan over the whole window, when the
// statistics held unbroken across it (scans_status ok) and the window is
// at least minUnusedWindowDays long.
func (b *builder) unused() {
	if b.days < minUnusedWindowDays {
		b.cov.Status = candidate.CoveragePartial
		b.cov.Note("the pack covers %d days, under the %d days an index must go unscanned "+
			"to be called unused, so none is", b.days, minUnusedWindowDays)
		return
	}
	if b.pf.status != "ok" {
		b.cov.Note("index scans are %s over the window (a statistics reset or too few "+
			"snapshots): whether an index is used is unknown, so none is called unused",
			b.pf.status)
		return
	}
	unknown := 0
	for _, f := range b.pf.facts {
		n, ok := scans(f)
		if !ok {
			unknown++
			continue
		}
		if n != 0 || !b.pf.droppable(f) || b.taken[factName(f)] || b.kept[factName(f)] ||
			!b.room(factTable(f), 0) {
			continue
		}
		b.add(b.drop(f, KindUnused, fmt.Sprintf("no index scan in the %d days of the pack "+
			"window, statistics unbroken", b.days), candidate.TierNone), factTable(f))
	}
	if unknown > 0 {
		b.cov.Note("%d indexes have unknown scans over the window", unknown)
	}
}
