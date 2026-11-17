package indexes

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/verify"
)

// set is a consolidation set: indexes whose keys lead cover's, which
// serves every lookup they serve. Dropping the members consolidates them
// into cover.
type set struct {
	cover   workpack.IndexFact
	members []workpack.IndexFact
}

func (s set) bytes() int64 {
	var n int64
	for _, m := range s.members {
		n += m.SizeBytes
	}
	return n
}

// consolidations proposes at most one set per table, the largest, each
// validated with m. Only a canceled context is an error.
func (b *builder) consolidations(ctx context.Context, m Measurer) error {
	byTable := map[string][]set{}
	var tables []string
	for _, s := range b.sets() {
		t := factTable(s.cover)
		if _, ok := byTable[t]; !ok {
			tables = append(tables, t)
		}
		byTable[t] = append(byTable[t], s)
	}
	slices.Sort(tables)
	for _, t := range tables {
		sets := byTable[t]
		slices.SortFunc(sets, largestFirst)
		if len(sets) > 1 {
			b.cov.Note("%s: one set per table per review; %d more wait for the next review",
				t, len(sets)-1)
		}
		if err := b.consolidate(ctx, m, sets[0]); err != nil {
			return err
		}
	}
	return nil
}

func largestFirst(a, b set) int {
	return cmp.Or(cmp.Compare(len(b.members), len(a.members)),
		cmp.Compare(b.bytes(), a.bytes()), cmp.Compare(factName(a.cover), factName(b.cover)))
}

// sets are the candidate consolidation sets: for each widest plain btree
// index (one no other index extends), the droppable plain btree indexes
// whose keys lead it (workpack's prefix_of), not already targeted.
func (b *builder) sets() []set {
	var out []set
	for _, cover := range b.pf.facts {
		if !plainBtree(cover) || len(cover.PrefixOf) > 0 || b.taken[factName(cover)] {
			continue
		}
		s := set{cover: cover}
		for _, f := range b.pf.facts {
			if plainBtree(f) && b.pf.droppable(f) && !b.taken[factName(f)] &&
				slices.Contains(f.PrefixOf, factName(cover)) {
				s.members = append(s.members, f)
			}
		}
		if len(s.members) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// consolidate validates s and adds its drops: steps when HypoPG planned
// the table's workload with the members hidden and nothing regressed past
// the bar, advice when it could not tell, nothing when it regressed.
func (b *builder) consolidate(ctx context.Context, m Measurer, s set) error {
	table := factTable(s.cover)
	var steps []candidate.Candidate
	for _, f := range s.members {
		if !b.room(table, len(steps)) {
			break
		}
		steps = append(steps, b.drop(f, KindConsolidation, fmt.Sprintf("its keys lead %s, "+
			"which serves the same lookups", factName(s.cover)),
			verify.TierName(verify.TierPlanOnly), s.cover))
	}
	if len(steps) == 0 {
		return nil
	}
	why, keep, err := b.validate(ctx, m, s, len(steps))
	if err != nil {
		return err
	}
	for _, c := range steps {
		switch {
		case !keep:
			continue
		case why != "":
			c = advise(c, why)
		default:
			c.Reason += fmt.Sprintf("; HypoPG planned the workload with the set hidden and "+
				"no statement rose more than %g%%", b.env.MinImprovementPct)
		}
		b.add(c, table)
	}
	return nil
}

// validate measures the set's first n members hidden. why is "" for a
// clean measurement, else why the set is only advice; keep is false when
// the set regressed and must not be proposed.
func (b *builder) validate(ctx context.Context, m Measurer, s set, n int,
) (why string, keep bool, err error) {
	if m == nil {
		return "hypopg is not available, so the set was not validated", true, nil
	}
	ds := DropSet{Schema: s.cover.Schema, Table: s.cover.Table,
		Queries:          tableQueries(b.pf.pack, s.cover.Schema, s.cover.Table),
		MaxRegressionPct: b.env.MinImprovementPct}
	for _, f := range s.members[:n] {
		ds.Indexes = append(ds.Indexes, f.Index)
	}
	res, err := m.MeasureDropSet(ctx, ds)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, fmt.Errorf("indexes generator: validate the set under %s: %w",
			factName(s.cover), ctxErr)
	}
	if err != nil {
		return fmt.Sprintf("HypoPG could not validate the set: %v", err), true, nil
	}
	return b.verdict(s, res)
}

// verdict reads a measurement of s.
func (b *builder) verdict(s set, res optimizer.WhatIfResult) (string, bool, error) {
	switch {
	case len(res.Regressions) > 0:
		r := res.Regressions[0]
		b.cov.Note("the set under %s is not proposed: hiding it raises the planned cost "+
			"of queryid %d by %.1f%% (more than %g%%)", factName(s.cover), r.QueryID, r.Pct,
			b.env.MinImprovementPct)
		return "", false, nil
	case res.OldIndexPresent:
		return "hypopg lacks hypopg_hide_index (1.4+), so the set was not measured", true,
			nil
	case res.Failed > 0:
		return fmt.Sprintf("hypopg could not plan queryids %v of the table's workload",
			res.UnplannedQueryIDs), true, nil
	case res.Measured == 0:
		return "hypopg planned no statement of the table's workload", true, nil
	}
	return "", true, nil
}
