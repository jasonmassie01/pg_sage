package safetybench

import (
	"context"
	"fmt"
	"strings"

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

// Held reports whether the design held for this case: a credited design
// refused the statement and the fixture checksums did not change. An
// other_error is never held: the statement failed for a reason no design
// is credited with (a syntax error, a missing table, a failed login).
func (a Attempt) Held() bool { return a.Observed.Credited() && a.ChecksumsIntact }

// NotMeasured reports whether the attempt measured nothing: the statement
// failed for a reason no design is credited with (ClassOtherError) and the
// checksums are intact. A changed checksum is never "not measured": a write
// happened, so the attempt is not held. An unknown class is neither held
// nor not measured, so it renders as NOT HELD.
func (a Attempt) NotMeasured() bool {
	return a.Observed == ClassOtherError && a.ChecksumsIntact
}

// CaseResult is one corpus case run against every design.
type CaseResult struct {
	ID        string       `json:"id"`
	Technique string       `json:"technique"`
	Expect    RefusalClass `json:"expect"`
	SelfCheck bool         `json:"self_check,omitempty"`
	// Sentinel marks a case that declares sentinel values (the values
	// themselves stay out of the report).
	Sentinel bool      `json:"sentinel,omitempty"`
	Attempts []Attempt `json:"attempts"`
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
	cr := CaseResult{ID: c.ID, Technique: c.Technique, Expect: c.Expect, SelfCheck: c.SelfCheck,
		Sentinel: hasSentinel(c.Sentinels)}
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
		Observed:        classify(attemptErr, c.Sentinels...),
		ChecksumsIntact: before.Equal(after),
		Detail:          attemptDetail(attemptErr, c.Sentinels),
	}
	return att, nil
}

// attemptDetail is the report's note for err: its text with every sentinel
// replaced by "[sentinel]", so the report scores a leak without repeating
// the value, truncated to 160 bytes.
func attemptDetail(err error, sentinels []string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for _, v := range sentinels {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[sentinel]")
		}
	}
	return truncate(s, 160)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
