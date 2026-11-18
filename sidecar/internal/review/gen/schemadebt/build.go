package schemadebt

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// builder collects one Build's candidates.
type builder struct {
	ctx  context.Context
	in   Inputs
	opts Options
	cov  *candidate.Coverage
	out  []candidate.Candidate
}

// Build is the generator as a pure function of the pack and the inputs:
// key and foreign key runways, timestamp columns, giant tables, then
// queue and soft-delete tables, every candidate validated against the
// pack. A pack without the schema section gives none and Coverage.Missing
// ["schema"]; debt columns without catalog facts make "catalog" missing.
// A nil pack, an invalid env or options and a canceled context are errors.
func Build(ctx context.Context, pack *workpack.Pack, in Inputs, env candidate.Env,
	opts Options) ([]candidate.Candidate, candidate.Coverage, error) {
	if err := checkInput(pack, env, opts); err != nil {
		return nil, candidate.Coverage{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, candidate.Coverage{}, fmt.Errorf("schemadebt generator for %s: %w",
			env.Database, err)
	}
	if opts.LargeTableBytes == 0 {
		opts.LargeTableBytes = DefaultLargeTableBytes
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	sch, ok, err := readSchema(pack)
	if err != nil {
		return nil, cov, fmt.Errorf("schemadebt generator for %s: %w", env.Database, err)
	}
	if !ok {
		cov.AddMissing("schema")
		return nil, cov, nil
	}
	idx, err := readIndexes(pack)
	if err != nil {
		return nil, cov, fmt.Errorf("schemadebt generator for %s: %w", env.Database, err)
	}
	b := &builder{ctx: ctx, in: in, opts: opts, cov: &cov}
	b.noteInputs(sch, in)
	if err := b.columns(findDebts(sch, readFamilies(pack))); err != nil {
		return nil, cov, fmt.Errorf("schemadebt generator for %s: %w", env.Database, err)
	}
	b.tables(sch, idx)
	if err := candidate.ValidateAll(pack, b.out); err != nil {
		return nil, cov, fmt.Errorf("schemadebt generator for %s: %w", env.Database, err)
	}
	return b.out, cov, nil
}

// noteInputs records a truncated schema or column read.
func (b *builder) noteInputs(sch workpack.Schema, in Inputs) {
	if sch.Truncated {
		b.cov.Status = candidate.CoveragePartial
		b.cov.Note("the schema section lists %d of %d tables", len(sch.Tables),
			sch.TablesTotal)
	}
	if in.Truncated {
		b.cov.Status = candidate.CoveragePartial
		b.cov.Note("catalog facts were read for at most %d columns", MaxColumns)
	}
}

// columns builds the key, foreign key and timestamp items in order.
func (b *builder) columns(d debts) error {
	if b.in.Columns == nil && len(d.columns()) > 0 {
		b.cov.AddMissing("catalog")
	}
	for _, k := range d.keys {
		if err := b.key(k); err != nil {
			return err
		}
	}
	for _, f := range d.fks {
		if err := b.foreignKey(f); err != nil {
			return err
		}
	}
	for _, t := range d.timestamps {
		if err := b.timestamp(t); err != nil {
			return err
		}
	}
	return nil
}

// column is the catalog facts of c and a note when they are missing.
func (b *builder) column(c ColumnRef) (Column, string) {
	col, ok := b.in.Columns[c]
	switch {
	case !ok:
		return Column{}, "the catalog facts of " + c.String() + " were not read"
	case col.ReadErr != "":
		return col, "reading the catalog facts of " + c.String() + " failed: " + col.ReadErr
	}
	return col, ""
}

// swap drafts the online column swap of c to target: the script, or why
// it was not drafted (a refusal of the planner or missing facts).
func (b *builder) swap(c ColumnRef, col Column, target string) (string, string, error) {
	if col.Facts.Table == "" {
		return "", "no catalog facts to plan an online column swap from", nil
	}
	p, err := plan.PlanColumnSwap(b.ctx, col.Facts, plan.SwapRequest{Schema: c.Schema,
		Table: c.Table, Column: c.Column, TargetType: target})
	if err != nil {
		if cerr := b.ctx.Err(); cerr != nil {
			return "", "", fmt.Errorf("plan the column swap of %s: %w", c, cerr)
		}
		return "", "online column swap refused: " + err.Error(), nil
	}
	return p.Script(), "", nil
}

// emit adds a candidate with its id, approval and the generator's names.
// Every schema-debt step needs a person's approval.
func (b *builder) emit(c candidate.Candidate) {
	c.Generator, c.Dimension = Name, Dimension
	c.Approval = candidate.ApprovalRequired
	c.ID = candidate.NewID(Name, c.Kind, c.Target, c.ForwardSQL)
	if c.Prediction.Tier == "" {
		c.Prediction.Tier = candidate.TierNone
	}
	b.out = append(b.out, c)
}

// swapCandidate is the planner's script as advice for a person to run: an
// operator candidate needs a setting landing the plan engine can watch
// (review-02b), and a column's type is not one.
func (b *builder) swapCandidate(c ColumnRef, script, why string, evidence []string) {
	b.emit(candidate.Candidate{Kind: KindColumnSwap, Target: c.String(),
		ActionType: candidate.ActionAdvice, ForwardSQL: script, EvidenceIDs: evidence,
		Reason: why + " The swap is an online migration plan (expand, backfill, " +
			"contract) for a person to run; pg_sage never runs it.",
		Prediction: candidate.Prediction{Metric: "column_type", Direction: "widen",
			Source: "migration planner (online column swap)"}})
}
