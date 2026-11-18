package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// The affected statements are measured on the clone before and after the
// expand steps (lab-04b) through the index rehearsal's guarded measurer
// (tuning.CloneMeasurer): executed only when proven read-only and
// unparameterized, planned otherwise. An executed statement runs up to
// measureRuns times and keeps its fastest run, so one cold read is not a
// regression.
const measureRuns = 3

// side is one statement's measurement on one side of the expand steps;
// skipped marks an after side not measured because the before side failed.
type side struct {
	got     tuning.CloneMeasurement
	err     error
	skipped bool
}

// affected is the workload to measure, or none with why.
func (runner *PostgresRehearsalRunner) affected(
	ctx context.Context, table planpkg.TableFacts,
) ([]AffectedStatement, string) {
	switch {
	case runner.workload == nil:
		return nil, "no production workload source is configured"
	case table.Name == "":
		return nil, "the plan names no table"
	}
	stmts, err := runner.workload(ctx, table)
	switch {
	case errors.Is(err, ErrNoStatementTexts):
		return nil, err.Error()
	case err != nil:
		return nil, "read the production workload: " + err.Error()
	case len(stmts) == 0:
		return nil, NoStatementReferences
	}
	return stmts[:min(len(stmts), MaxAffectedStatements)], ""
}

// measureSide measures every statement on one clone connection; with
// before set, a statement whose before side failed is skipped.
func (runner *PostgresRehearsalRunner) measureSide(ctx context.Context,
	pool *pgxpool.Pool, stmts []AffectedStatement, before []side) ([]side, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to the rehearsal clone to measure: %w", err)
	}
	defer conn.Release()
	m, err := tuning.NewCloneMeasurer(ctx, conn.Conn(), runner.analyzeTimeout,
		runner.inspect)
	if err != nil {
		return nil, err
	}
	out := make([]side, len(stmts))
	for i, s := range stmts {
		if before != nil && before[i].err != nil {
			out[i].skipped = true
			continue
		}
		out[i] = measureBest(ctx, m, s.Text)
	}
	return out, nil
}

// measureBest measures text once, and an executed statement up to
// measureRuns times, keeping the fastest run.
func measureBest(ctx context.Context, m tuning.CloneMeasurer, text string) side {
	got, err := m.Measure(ctx, text)
	if err != nil || !got.Executed() {
		return side{got: got, err: err}
	}
	for run := 1; run < measureRuns; run++ {
		again, err := m.Measure(ctx, text)
		if err != nil || !again.Executed() {
			break
		}
		if again.LatencyMS < got.LatencyMS {
			got = again
		}
	}
	return side{got: got}
}

// pairAll pairs each statement's two sides.
func pairAll(stmts []AffectedStatement, before, after []side) []rehearsalpkg.QueryMeasurement {
	out := make([]rehearsalpkg.QueryMeasurement, 0, len(stmts))
	for i, s := range stmts {
		out = append(out, pairQuery(s.QueryID, before[i], after[i]))
	}
	return out
}

// pairQuery compares like with like: latency and blocks when both sides
// executed, plan cost when either was planned only. A failed side leaves
// the statement unmeasured with why.
func pairQuery(id int64, before, after side) rehearsalpkg.QueryMeasurement {
	q := rehearsalpkg.QueryMeasurement{QueryID: id}
	if before.err != nil {
		q.Unmeasured, q.TimedOut = unmeasured(before.err, false)
		return q
	}
	q.BeforeCost, q.NotExecuted = before.got.Cost, before.got.NotExecuted
	if before.got.Executed() {
		q.BeforeLatencyMS, q.BeforeBlocks = before.got.LatencyMS, before.got.Blocks
	}
	if q.BeforePlanHash, q.Unmeasured = planHash(before.got.Plan); q.Unmeasured != "" {
		return q
	}
	if after.err != nil || after.skipped {
		q.Unmeasured, q.TimedOut = unmeasured(after.err, true)
		return q
	}
	q.AfterCost = after.got.Cost
	if q.AfterPlanHash, q.Unmeasured = planHash(after.got.Plan); q.Unmeasured != "" {
		return q
	}
	if before.got.Executed() && after.got.Executed() {
		q.AfterLatencyMS, q.AfterBlocks = after.got.LatencyMS, after.got.Blocks
		return q
	}
	q.BeforeLatencyMS, q.BeforeBlocks = 0, nil
	if q.NotExecuted == "" {
		q.NotExecuted = after.got.NotExecuted
	}
	return q
}

// unmeasured is why a failed side gives no evidence, and whether it
// timed out.
func unmeasured(err error, afterSteps bool) (string, bool) {
	when := "before"
	if afterSteps {
		when = "after"
	}
	switch {
	case errors.Is(err, tuning.ErrStatementTimedOut) && afterSteps:
		return rehearsalpkg.UnmeasuredTimedOutAfter, true
	case errors.Is(err, tuning.ErrStatementTimedOut):
		return rehearsalpkg.UnmeasuredTimedOutBefore, true
	case errors.Is(err, tuning.ErrNotOneStatement):
		return rehearsalpkg.UnmeasuredNotOne, false
	case err == nil:
		return "not measured " + when + " the expand steps", false
	case afterSteps:
		return fmt.Sprintf("%s: %v", rehearsalpkg.UnmeasuredFailedAfter, err), false
	}
	return fmt.Sprintf("measure %s the expand steps: %v", when, err), false
}

func planHash(plan []byte) (string, string) {
	hash, err := planhash.Compute(plan)
	if err != nil {
		return "", "plan hash: " + err.Error()
	}
	return hash, ""
}
