// Package schemadebt is the schema-debt generator of Sage Review
// (review-02d): it joins the shapes the schema lint finds to a rate. An
// int4 key or a foreign key narrower than its key gets the runway of the
// sequence that feeds it, fitted here from sage.runway_samples ("exhausts
// in 247 days", with its error; fewer than 3 samples is "rate not
// measured yet", never a guess). Timestamp without time zone columns that
// top queries read, giant unpartitioned tables with a time-ordered key
// and queue or soft-delete tables with high churn are named too.
//
// Nothing here executes. An int4 key under ProposalRunwayDays of runway
// is a prepare_sequence_capacity_migration proposal (its script is the
// sequence playbook's; the contract is forward-fix only, so it always
// needs approval); a type change is an online column swap drafted by the
// migration planner, offered as an operator step a person approves; the
// rest is advice.
package schemadebt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves.
const (
	Name      = "schemadebt"
	Dimension = "schema"
)

// Candidate kinds.
const (
	KindInt4Key       = "int4_key"
	KindFKMismatch    = "fk_type_mismatch"
	KindColumnSwap    = "column_swap"
	KindTimestampNoTZ = "timestamp_without_time_zone"
	KindLargeTable    = "large_unpartitioned_table"
	KindQueueTable    = "queue_table"
	KindSoftDelete    = "soft_delete_table"
)

// Prediction quantities.
const (
	QuantityRunwayDays = "runway_days"
	QuantityRatePerDay = "values_per_day"
	QuantityRemaining  = "remaining_values"
	QuantitySamples    = "samples"
	QuantityMeasured   = "runway_measured"
	QuantityInt4Key    = "int4_key"
)

// ActionCapacityMigration is the executor contract of an int4 key under
// ProposalRunwayDays of runway.
const ActionCapacityMigration = "prepare_sequence_capacity_migration"

// ProposalRunwayDays: a measured runway under this many days is a
// capacity proposal; at or above it, advice.
const ProposalRunwayDays = 365

// DefaultLargeTableBytes is review.large_table_bytes' default, used when
// Options leaves it zero.
const DefaultLargeTableBytes int64 = 10 << 30

// Options configure the generator. LargeTableBytes is
// review.large_table_bytes (0: DefaultLargeTableBytes).
type Options struct {
	LargeTableBytes int64
}

// Generator is the schema-debt generator. Its source reads the catalog
// facts and runway samples; a nil source reads nothing, so no runway is
// measured and no swap is planned.
type Generator struct {
	src  Source
	opts Options
}

// New is a generator reading through src (nil: nothing) with opts.
func New(src Source, opts Options) *Generator { return &Generator{src: src, opts: opts} }

// Name is "schemadebt".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate finds the pack's debt columns, has the source read their
// facts and samples since the pack window's start, and builds the
// candidates (Build). A source error is the generator's error.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if pack == nil {
		return nil, candidate.Coverage{}, errors.New("schemadebt generator: no evidence pack")
	}
	sch, ok, err := readSchema(pack)
	if err != nil || !ok {
		return Build(ctx, pack, Inputs{}, env, g.opts)
	}
	var in Inputs
	cols := findDebts(sch, readFamilies(pack)).columns()
	if g.src != nil && len(cols) > 0 {
		if in, err = g.src.Read(ctx, cols, pack.Window.From); err != nil {
			return nil, candidate.Coverage{}, fmt.Errorf(
				"schemadebt generator for %s: read the catalog and runway samples: %w",
				env.Database, err)
		}
	}
	cands, cov, err := Build(ctx, pack, in, env, g.opts)
	if g.src == nil && len(cols) > 0 {
		cov.AddMissing("runway_samples")
	}
	return cands, cov, err
}

// readSchema decodes the schema section; ok is false without one.
func readSchema(pack *workpack.Pack) (workpack.Schema, bool, error) {
	raw := pack.File("schema.json")
	if raw == nil {
		return workpack.Schema{}, false, nil
	}
	var sch workpack.Schema
	if err := json.Unmarshal(raw, &sch); err != nil {
		return workpack.Schema{}, false, fmt.Errorf("decode schema.json: %w", err)
	}
	return sch, true, nil
}

// readFamilies is the workload's families; a pack always has the section.
func readFamilies(pack *workpack.Pack) []workpack.Family {
	var wl workpack.Workload
	if err := json.Unmarshal(pack.File("workload.json"), &wl); err != nil {
		return nil
	}
	return wl.Families
}

// readIndexes is the index section's facts (nil without one).
func readIndexes(pack *workpack.Pack) ([]workpack.IndexFact, error) {
	raw := pack.File("indexes.json")
	if raw == nil {
		return nil, nil
	}
	var sec workpack.Indexes
	if err := json.Unmarshal(raw, &sec); err != nil {
		return nil, fmt.Errorf("decode indexes.json: %w", err)
	}
	return sec.Indexes, nil
}

// checkInput refuses what no review could have built.
func checkInput(pack *workpack.Pack, env candidate.Env, opts Options) error {
	switch {
	case pack == nil:
		return errors.New("schemadebt generator: no evidence pack")
	case env.Database == "":
		return errors.New("schemadebt generator: no database named")
	case env.WindowDays < 1:
		return fmt.Errorf("schemadebt generator for %s: window of %d days", env.Database,
			env.WindowDays)
	case opts.LargeTableBytes < 0:
		return fmt.Errorf("schemadebt generator for %s: review.large_table_bytes %d is "+
			"negative", env.Database, opts.LargeTableBytes)
	}
	return nil
}

// Totals are the review's numbers over this generator's candidates: debt
// items with a measured runway, int4 keys, and int4 keys measured.
func Totals(cands []candidate.Candidate) (measured, keys, keysMeasured int) {
	for _, c := range cands {
		if c.Generator != Name {
			continue
		}
		q := c.Prediction.Quantities
		if q[QuantityMeasured] == 1 {
			measured++
		}
		if q[QuantityInt4Key] == 1 {
			keys++
			if q[QuantityMeasured] == 1 {
				keysMeasured++
			}
		}
	}
	return measured, keys, keysMeasured
}
