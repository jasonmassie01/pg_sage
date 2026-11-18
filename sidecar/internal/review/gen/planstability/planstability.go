// Package planstability is Sage Review's plan-stability generator
// (review-02c). For the pack's top queries by total time it reads the
// columns their predicates filter on (from the normalized text) and flags
// four statistical cliffs the planner can fall off: a tiny non-null share,
// skew beyond the sample, time-ordered inserts past the histogram and
// correlated predicates. A cliff column gets a set_column_statistics step
// (10x its target, at most 10000) and the ANALYZE that builds it;
// correlated columns get pg_sage's extended statistics. A table whose
// statistics pg_sage cannot read is unknown, never "no risk".
package planstability

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves.
const (
	Name      = "planstability"
	Dimension = "workload_stability"
)

// Candidate kinds.
const (
	KindNullCliff   = "null_cliff"
	KindSkew        = "skew_cliff"
	KindTimeOrdered = "time_ordered_cliff"
	KindCorrelated  = "correlated_predicates"
	KindAnalyze     = "statistics_analyze"
	KindAtMaximum   = "statistics_target_at_maximum"
	KindSwitchedOff = "statistics_switched_off"
)

// The cliffs' bounds.
const (
	// NullFracCliff: a column this NULL, filtered with = or IS NOT NULL.
	NullFracCliff = 0.999
	// SkewTopFreq and SkewDistinct: the leading most-common frequency
	// above the first, with more distinct values than the second.
	SkewTopFreq  = 0.5
	SkewDistinct = 1000
	// CorrelationCliff and ModShare: physical order above the first, with
	// more than the second share of the rows changed since ANALYZE.
	CorrelationCliff = 0.95
	ModShare         = 0.10
	// EstimateErrorCliff: a stored plan's estimate off by more than this.
	EstimateErrorCliff = 10
	// TargetFactor raises a target; MaxTopQueries bounds the queries read.
	TargetFactor  = 10
	MaxTopQueries = 20
)

// Generator is the plan-stability generator. It keeps no state.
type Generator struct {
	src Source
}

// New reads the catalog through src.
func New(src Source) *Generator { return &Generator{src: src} }

// Name is "planstability".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate judges the pack's top queries. No top queries (or none whose
// text names a filter) gives no candidates and reads nothing. A nil pack,
// an invalid env, no source, a canceled context and a failed catalog read
// are errors.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if err := g.check(ctx, pack, env); err != nil {
		return nil, candidate.Coverage{}, err
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	in, err := readPack(pack, &cov)
	if err != nil {
		return nil, cov, fmt.Errorf("planstability generator for %s: %w", env.Database, err)
	}
	queries := in.parse(&cov)
	tables := filteredTables(queries)
	if len(tables) == 0 {
		return nil, cov, nil
	}
	facts, err := g.src.Read(ctx, tables)
	if err != nil {
		return nil, cov, fmt.Errorf("planstability generator for %s: read the statistics "+
			"of %v: %w", env.Database, tables, err)
	}
	j := judge{in: in, facts: facts, cov: &cov, cliffs: map[string]*cliff{}}
	j.unknownTables(tables)
	for _, q := range queries {
		j.query(q)
	}
	out := j.candidates()
	if err := candidate.ValidateAll(pack, out); err != nil {
		return nil, cov, fmt.Errorf("planstability generator for %s: %w", env.Database, err)
	}
	return out, cov, nil
}

func (g *Generator) check(ctx context.Context, pack *workpack.Pack, env candidate.Env) error {
	switch {
	case pack == nil:
		return errors.New("planstability generator: no evidence pack")
	case g.src == nil:
		return errors.New("planstability generator: no catalog source")
	case env.Database == "" || env.WindowDays < 1:
		return fmt.Errorf("planstability generator: env names database %q and a window "+
			"of %d days", env.Database, env.WindowDays)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("planstability generator for %s: %w", env.Database, err)
	}
	return nil
}

// filteredTables are the relations of the queries with a predicate.
func filteredTables(queries []parsedQuery) []string {
	seen := map[string]bool{}
	var out []string
	for _, q := range queries {
		if len(q.preds) == 0 {
			continue
		}
		for _, rel := range q.rels {
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	sort.Strings(out)
	return out
}
