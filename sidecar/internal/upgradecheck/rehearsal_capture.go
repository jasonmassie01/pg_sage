package upgradecheck

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/tuning"
	"github.com/pg-sage/sidecar/internal/workload"
)

// What a rehearsal measures of each top statement: only a statement
// explain.ReadOnlyStatement admits is ever sent to the clone, and the
// clone measurer (tuning.CloneMeasurer) executes it with EXPLAIN
// (ANALYZE, BUFFERS) only when the analyze guard proves it read-only, in a
// READ ONLY rolled-back transaction under the statement timeout. A
// statement the guard refuses is skipped, never executed. A statement
// with $n parameters is planned with EXPLAIN (GENERIC_PLAN) when both
// majors are PG16 or later, else it is not comparable.

// maxRehearsalText bounds a statement's text in the report.
const maxRehearsalText = 2000

// minGenericPlanMajor is the first major with EXPLAIN (GENERIC_PLAN).
const minGenericPlanMajor = 16

var paramPlaceholder = regexp.MustCompile(`\$[0-9]+`)

func hasParams(text string) bool {
	return paramPlaceholder.MatchString(workload.BlankLiteralsAndComments(text))
}

// classifyTopQuery is "" when text is measured, else its status and why.
func classifyTopQuery(text string, sourceMajor, targetMajor int) (string, string) {
	if _, err := explain.ReadOnlyStatement(text); err != nil {
		return QuerySkipped, "not provably read-only, never sent to the clone: " + err.Error()
	}
	if hasParams(text) && min(sourceMajor, targetMajor) < minGenericPlanMajor {
		return QueryNotComparable, fmt.Sprintf("has $n parameters: a generic plan needs "+
			"PG%d on both majors (source PG%d, target PG%d)", minGenericPlanMajor,
			sourceMajor, targetMajor)
	}
	return "", ""
}

func prepareQueries(top []TopQuery, sourceMajor, targetMajor int) []RehearsedQuery {
	out := make([]RehearsedQuery, len(top))
	for i, t := range top {
		text := t.Text
		if len(text) > maxRehearsalText {
			text = strings.ToValidUTF8(text[:maxRehearsalText], "")
		}
		out[i] = RehearsedQuery{QueryID: t.QueryID, Text: text, Status: QueryCompared}
		if status, why := classifyTopQuery(t.Text, sourceMajor, targetMajor); status != "" {
			out[i].Status, out[i].Reason = status, why
		}
	}
	return out
}

// captureSlot picks which of a statement's captures a pass fills.
type captureSlot func(*RehearsedQuery) **PlanCapture

func before(q *RehearsedQuery) **PlanCapture       { return &q.Before }
func asCutOver(q *RehearsedQuery) **PlanCapture    { return &q.AsCutOver }
func afterAnalyze(q *RehearsedQuery) **PlanCapture { return &q.AfterAnalyze }

// capture measures every compared statement on cl, after checking that cl
// is not production and answers at wantMajor.
func (r *rehearsalRun) capture(ctx context.Context, cl clone.Clone, wantMajor int,
	queries []RehearsedQuery, slot captureSlot) error {
	pool, err := clone.GuardedConnect(ctx, cl, r.opts.Production)
	if err != nil {
		return fmt.Errorf("connect the rehearsal clone %s: %w", cl.ID, err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect the rehearsal clone %s: %w", cl.ID, err)
	}
	defer conn.Release()
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		return fmt.Errorf("read the rehearsal clone's version: %w", err)
	}
	if version/10000 != wantMajor {
		return fmt.Errorf("the rehearsal clone %s answered as PostgreSQL %d, want %d",
			cl.ID, version/10000, wantMajor)
	}
	m, err := tuning.NewCloneMeasurer(ctx, conn.Conn(), r.opts.StatementTimeout,
		r.opts.Inspect)
	if err != nil {
		return err
	}
	for i := range queries {
		if queries[i].Status == QueryCompared {
			measureInto(ctx, m, &queries[i], slot)
		}
	}
	return nil
}

// measureInto fills q's capture; a statement the analyze guard refuses is
// skipped from then on and keeps no capture.
func measureInto(ctx context.Context, m tuning.CloneMeasurer, q *RehearsedQuery,
	slot captureSlot) {
	got, err := m.Measure(ctx, q.Text)
	if got.NotExecuted != "" && !hasParams(q.Text) {
		q.Status, q.Reason = QuerySkipped, got.NotExecuted
		q.Before, q.AsCutOver, q.AfterAnalyze = nil, nil, nil
		return
	}
	c := &PlanCapture{Cost: got.Cost, BlocksPerCall: got.Blocks, NotExecuted: got.NotExecuted}
	if err != nil {
		c.Error = err.Error()
	} else if hash, herr := planhash.Compute(got.Plan); herr != nil {
		c.Error = "fingerprint the plan: " + herr.Error()
	} else {
		c.PlanHash = hash
	}
	*slot(q) = c
}

// fingerprinted is a capture with a plan fingerprint.
func fingerprinted(c *PlanCapture) bool { return c != nil && c.Error == "" && c.PlanHash != "" }

func blocksDelta(from, to *PlanCapture) *int64 {
	if !fingerprinted(from) || !fingerprinted(to) || from.BlocksPerCall == nil ||
		to.BlocksPerCall == nil {
		return nil
	}
	d := *to.BlocksPerCall - *from.BlocksPerCall
	return &d
}

// compareCaptures sets q's change flags and blocks deltas against the
// capture before the upgrade; a statement no capture could judge is an
// error with the first capture error as its reason.
func compareCaptures(q *RehearsedQuery) {
	if q.Status != QueryCompared {
		return
	}
	b := q.Before
	q.ChangedAsCutOver = fingerprinted(b) && fingerprinted(q.AsCutOver) &&
		b.PlanHash != q.AsCutOver.PlanHash
	q.ChangedAfterAnalyze = fingerprinted(b) && fingerprinted(q.AfterAnalyze) &&
		b.PlanHash != q.AfterAnalyze.PlanHash
	q.BlocksDeltaAsCutOver = blocksDelta(b, q.AsCutOver)
	q.BlocksDeltaAfterAnalyze = blocksDelta(b, q.AfterAnalyze)
	if q.PlanChanged() {
		return
	}
	for _, c := range []*PlanCapture{b, q.AsCutOver, q.AfterAnalyze} {
		if c != nil && c.Error != "" {
			q.Status, q.Reason = QueryError, c.Error
			return
		}
	}
}

// summarize lists the changed and the not compared statements.
func summarize(rep *RehearsalReport, queries []RehearsedQuery) {
	rep.Changed, rep.NotCompared = nil, nil
	rep.Unchanged, rep.PlansChanged, rep.RecoveredAfterAnalyze = 0, 0, 0
	if len(queries) == 0 {
		rep.Note = NothingToCompare
		return
	}
	for _, q := range queries {
		switch {
		case q.Status != QueryCompared:
			rep.NotCompared = append(rep.NotCompared, q)
		case q.PlanChanged():
			rep.Changed = append(rep.Changed, q)
			rep.PlansChanged++
			if q.RecoveredAfterAnalyze() {
				rep.RecoveredAfterAnalyze++
			}
		default:
			rep.Unchanged++
		}
	}
}
