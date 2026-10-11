package safetybench

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Attempt is the outcome of one (case, design) pair.
type Attempt struct {
	Design          string       `json:"design"`
	Observed        RefusalClass `json:"observed"`
	ChecksumsIntact bool         `json:"checksums_intact"`
	// Detail is the refusal reason or the execution note, for the report.
	Detail string `json:"detail,omitempty"`
}

// Held reports whether the design held for this case: the statement was
// refused and the fixture checksums did not change.
func (a Attempt) Held() bool { return a.Observed.Refused() && a.ChecksumsIntact }

// CaseResult is one corpus case run against every design.
type CaseResult struct {
	ID        string       `json:"id"`
	Technique string       `json:"technique"`
	Expect    RefusalClass `json:"expect"`
	SelfCheck bool         `json:"self_check,omitempty"`
	Attempts  []Attempt    `json:"attempts"`
}

// RunReadOnly runs every case against every design on owner's database.
// Before each attempt it snapshots the fixture checksums; after, it
// re-snapshots and records whether they are intact. The fixture is rebuilt
// before the run so results do not depend on prior cases.
func RunReadOnly(
	ctx context.Context, owner *pgxpool.Pool, cases []Case, designs []RODesign,
) ([]CaseResult, error) {
	if err := PrepareReadOnly(ctx, owner); err != nil {
		return nil, err
	}
	tables := fixtureTables()
	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		cr, err := runCase(ctx, owner, c, designs, tables)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", c.ID, err)
		}
		results = append(results, cr)
	}
	return results, nil
}

// runCase runs one case against every design, checksumming around each
// attempt.
func runCase(
	ctx context.Context, owner *pgxpool.Pool, c Case, designs []RODesign, tables []string,
) (CaseResult, error) {
	cr := CaseResult{ID: c.ID, Technique: c.Technique, Expect: c.Expect, SelfCheck: c.SelfCheck}
	for _, d := range designs {
		att, err := runAttempt(ctx, owner, c, d, tables)
		if err != nil {
			return cr, fmt.Errorf("design %s: %w", d.Name(), err)
		}
		cr.Attempts = append(cr.Attempts, att)
	}
	return cr, nil
}

// runAttempt snapshots, runs one design's attempt, re-snapshots and
// classifies the outcome.
func runAttempt(
	ctx context.Context, owner *pgxpool.Pool, c Case, d RODesign, tables []string,
) (Attempt, error) {
	before, err := snapshot(ctx, owner, tables)
	if err != nil {
		return Attempt{}, err
	}
	attemptErr := d.Attempt(ctx, owner, c.SQL)
	after, err := snapshot(ctx, owner, tables)
	if err != nil {
		return Attempt{}, err
	}
	att := Attempt{
		Design:          d.Name(),
		Observed:        classify(attemptErr),
		ChecksumsIntact: before.Equal(after),
	}
	if attemptErr != nil {
		att.Detail = truncate(attemptErr.Error(), 160)
	}
	return att, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
