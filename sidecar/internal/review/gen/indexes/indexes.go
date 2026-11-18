// Package indexes is the index portfolio generator of Sage Review
// (review-02a): it judges a database's indexes as a set over the whole
// pack window. It proposes dropping exact duplicates, indexes unused over
// the whole window, and consolidation sets (indexes whose keys lead a
// wider index of the same table), validated with HypoPG as one change.
// Every SQL is rendered by the analyzer's drop builder; every candidate
// cites the pack's index facts and goes through candidate.Validate.
package indexes

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves.
const (
	Name      = "indexes"
	Dimension = "schema"
)

// Candidate kinds.
const (
	KindDuplicate     = "duplicate"
	KindConsolidation = "consolidation"
	KindUnused        = "unused"
)

// Prediction metrics and quantities.
const (
	MetricWriteBlocks   = "index_write_blocks_per_day"
	MetricIndexBytes    = "index_bytes"
	QuantityBytes       = "bytes_reclaimed"
	QuantityWriteBlocks = "write_blocks_saved_per_day"
)

// Generator is the index portfolio generator. Its measurer validates
// consolidation sets; a nil measurer means no HypoPG, so a set is advice.
type Generator struct{ m Measurer }

// New is a generator that validates sets with m (nil: no HypoPG).
func New(m Measurer) *Generator { return &Generator{m: m} }

// Name is "indexes".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate reads the pack's index section and proposes, in order: exact
// duplicate drops, at most one consolidation set per table, then drops of
// indexes unused over the whole window. A pack without the index section
// gives no candidates and Coverage.Missing ["indexes"]. A nil pack, an
// invalid env and a canceled context are errors; a HypoPG failure only
// downgrades a set to advice.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if pack == nil {
		return nil, candidate.Coverage{}, errors.New("indexes generator: no evidence pack")
	}
	if err := checkEnv(env); err != nil {
		return nil, candidate.Coverage{}, err
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	pf, ok, err := readPortfolio(pack)
	if err != nil {
		return nil, cov, fmt.Errorf("indexes generator for %s: %w", env.Database, err)
	}
	if !ok {
		cov.AddMissing("indexes")
		return nil, cov, nil
	}
	b := newBuilder(pf, env, &cov)
	b.duplicates()
	if err := b.consolidations(ctx, g.m); err != nil {
		return nil, cov, err
	}
	b.unused()
	if err := candidate.ValidateAll(pack, b.out); err != nil {
		return nil, cov, fmt.Errorf("indexes generator for %s: %w", env.Database, err)
	}
	return b.out, cov, nil
}

// checkEnv refuses an env no review could have built.
func checkEnv(env candidate.Env) error {
	switch {
	case env.Database == "":
		return errors.New("indexes generator: no database named")
	case env.WindowDays < 1:
		return fmt.Errorf("indexes generator for %s: window of %d days", env.Database,
			env.WindowDays)
	case env.MinImprovementPct < 0 || env.MaxIndexesPerTable < 0:
		return fmt.Errorf("indexes generator for %s: negative bar %g or cap %d",
			env.Database, env.MinImprovementPct, env.MaxIndexesPerTable)
	}
	return nil
}

// Totals are the review's numbers: index bytes reclaimable and predicted
// write blocks saved a day, over this generator's steps (advice excluded).
func Totals(cands []candidate.Candidate) (bytes int64, writeBlocks float64) {
	for _, c := range cands {
		if c.Generator != Name || c.ActionType == candidate.ActionAdvice {
			continue
		}
		bytes += int64(c.Prediction.Quantities[QuantityBytes])
		writeBlocks += c.Prediction.Quantities[QuantityWriteBlocks]
	}
	return bytes, writeBlocks
}
