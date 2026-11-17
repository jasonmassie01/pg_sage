package schemadebt

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// typeLimit is how far an integer type reaches in the series' rising
// direction: its maximum, or minus its minimum for a descending sequence.
func typeLimit(typ string, descending bool) float64 {
	limits := map[string][2]float64{"smallint": {32767, 32768},
		"integer": {2147483647, 2147483648}, "bigint": {9223372036854775807,
			9223372036854775808}}
	l := limits[typ]
	if descending {
		return l[1]
	}
	return l[0]
}

// runwayOf fits the samples of col's sequence against limitType.
func (b *builder) runwayOf(col Column, limitType string) (Runway, Series) {
	desc := col.Increment < 0
	s := b.in.Series[col.Sequence]
	if col.Sequence == "" {
		s = Series{}
	}
	return Fit(s, typeLimit(limitType, desc), desc), s
}

// runwayPrediction is a runway's prediction: its quantities and the
// samples it cites.
func runwayPrediction(r Runway, s Series, seq string, key bool) candidate.Prediction {
	q := map[string]float64{QuantitySamples: float64(r.Samples), QuantityMeasured: 0}
	if key {
		q[QuantityInt4Key] = 1
	}
	if r.Measured && r.Days >= 0 {
		q[QuantityMeasured] = 1
		q[QuantityRunwayDays] = float64(r.Days)
		q[QuantityRatePerDay] = r.RatePerDay
		q[QuantityRemaining] = r.Remaining
	}
	src := "sage.runway_samples: no sequence feeds the column"
	switch {
	case seq != "" && r.Samples > 0:
		pts := finitePoints(s.Points)
		src = fmt.Sprintf("sage.runway_samples sequence %s epoch %s: %d samples from %s "+
			"to %s", seq, s.Epoch, r.Samples, pts[0].At.UTC().Format(time.DateOnly),
			pts[len(pts)-1].At.UTC().Format(time.DateOnly))
	case seq != "":
		src = "sage.runway_samples: no samples of sequence " + seq + " in the window"
	}
	return candidate.Prediction{Metric: QuantityRunwayDays, Direction: "increase",
		Source: src, Quantities: q}
}

// key is an int4 key's item: a capacity proposal under ProposalRunwayDays
// of measured runway, advice otherwise; plus its column swap when the
// planner drafts one.
func (b *builder) key(k keyDebt) error {
	col, missing := b.column(k.ref)
	if missing == "" && col.Sequence == "" {
		b.cov.Note("no sequence feeds the %s key %s; no runway to measure", k.colType, k.ref)
		return nil
	}
	r, s := b.runwayOf(col, k.colType)
	script, refusal, err := b.swap(k.ref, col, "bigint")
	if err != nil {
		return err
	}
	ev := []string{k.table.ID}
	reason := fmt.Sprintf("%s, the %s primary key fed by sequence %s, %s.", k.ref,
		k.colType, orUnknown(col.Sequence), r.Statement())
	c := candidate.Candidate{Kind: KindInt4Key, Target: k.ref.String(),
		ActionType: candidate.ActionAdvice, EvidenceIDs: ev,
		Prediction: runwayPrediction(r, s, col.Sequence, true)}
	if r.Measured && r.Days >= 0 && r.Days < ProposalRunwayDays {
		c.ActionType = ActionCapacityMigration
		c.ForwardSQL = cases.SequenceCapacityScript(sequenceName(col), rateDetail(r, col))
		reason += " Under 365 days of runway: review the forward-fix capacity migration."
	}
	c.Reason = withNotes(reason, missing, refusal)
	b.emit(c)
	if script != "" {
		b.swapCandidate(k.ref, script, "Widen "+k.ref.String()+" to bigint online.", ev)
	}
	return nil
}

// foreignKey is a narrow foreign key column's item: advice with the
// runway of the referenced key's sequence against the column's own type,
// plus its column swap when the planner drafts one.
func (b *builder) foreignKey(f fkDebt) error {
	parent, pMissing := b.column(f.parent)
	r, s := b.runwayOf(parent, f.childType)
	child, cMissing := b.column(f.child)
	script, refusal, err := b.swap(f.child, child, f.parentType)
	if err != nil {
		return err
	}
	ev := []string{f.childTab.ID, f.parentTab.ID}
	reason := fmt.Sprintf("%s is %s but references %s, which is %s (foreign key %s): at "+
		"the rate of sequence %s, the column %s.", f.child, f.childType, f.parent,
		f.parentType, f.constraint, orUnknown(parent.Sequence), r.Statement())
	b.emit(candidate.Candidate{Kind: KindFKMismatch, Target: f.child.String(),
		ActionType: candidate.ActionAdvice, EvidenceIDs: ev,
		Reason:     withNotes(reason, pMissing, cMissing, refusal),
		Prediction: runwayPrediction(r, s, parent.Sequence, false)})
	if script != "" {
		b.swapCandidate(f.child, script, fmt.Sprintf("Widen %s to %s online.", f.child,
			f.parentType), ev)
	}
	return nil
}

// sequenceName is the sequence by its catalog names for the playbook's
// script, or the runway subject when the facts do not name it.
func sequenceName(col Column) string {
	if col.SequenceName != "" {
		return col.SequenceSchema + "." + col.SequenceName
	}
	if c := col.Facts.Column; c.SequenceName != "" {
		return c.SequenceSchema + "." + c.SequenceName
	}
	return strings.ReplaceAll(col.Sequence, `"`, "")
}

// rateDetail is the runway as the sequence playbook's forecast detail.
func rateDetail(r Runway, col Column) map[string]any {
	return map[string]any{cases.DetailRunwayRatePerS: r.RatePerDay / 86400,
		cases.DetailRunwaySecondsToLimit: r.Remaining / r.RatePerDay * 86400,
		cases.DetailRunwayReadAt:         r.To, "increment": float64(col.Increment)}
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// withNotes appends the non-empty notes as sentences.
func withNotes(reason string, notes ...string) string {
	for _, n := range notes {
		if n != "" {
			reason += " Note: " + n + "."
		}
	}
	return reason
}
