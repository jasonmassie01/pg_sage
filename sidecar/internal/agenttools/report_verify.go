package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/verify"
)

const reportByFindingSQL = `/* pg_sage */ SELECT ` + reportColumns + `
FROM sage.source_fix WHERE finding_id = $1`

// decideSQL stores a verdict once: only an undecided deployed report.
const decideSQL = `/* pg_sage */ UPDATE sage.source_fix SET stage = 'verified',
    verdict = $2, tolerance = $3, observed = $4, evidence = $5, reason = $6,
    decided_at = $7, updated_at = now()
WHERE finding_id = $1 AND stage = 'deployed' AND decided_at IS NULL AND deployed_at = $8
RETURNING ` + reportColumns

// decision is a verdict on a deploy, ready to store.
type decision struct {
	verdict, tolerance, reason string
	observed                   verify.Observed
	evidence                   map[string]any
}

func (t *Tools) readReport(ctx context.Context, findingID int64) (reportRow, error) {
	row, err := scanReport(t.pool.QueryRow(ctx, reportByFindingSQL, findingID))
	if errors.Is(err, pgx.ErrNoRows) {
		return reportRow{}, fmt.Errorf("%w: no source-fix report for finding %d",
			ErrNotFound, findingID)
	}
	if err != nil {
		return reportRow{}, fmt.Errorf("read report of finding %d: %w", findingID, err)
	}
	return row, nil
}

// reportStatus returns the report, deciding its verdict when the deploy's
// verify window has closed. Concurrent callers may all measure; the
// conditional update stores exactly one verdict and everyone returns it.
func (t *Tools) reportStatus(ctx context.Context, findingID int64) (Report, error) {
	row, err := t.readReport(ctx, findingID)
	if err != nil {
		return Report{}, err
	}
	if row.Stage != StageDeployed || row.Verdict != nil || row.DeployedAt == nil ||
		t.now().Before(row.DeployedAt.Add(t.opts.VerifyWindow)) {
		return t.toReport(row)
	}
	d, err := t.decide(ctx, row)
	if err != nil {
		return Report{}, err
	}
	observed, evidence, err := encodeDecision(d)
	if err != nil {
		return Report{}, err
	}
	stored, err := scanReport(t.pool.QueryRow(ctx, decideSQL, findingID, d.verdict,
		d.tolerance, observed, evidence, clipBytes(d.reason, 2000), t.now(), *row.DeployedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		// Another caller decided first (or the deploy was re-reported).
		return t.reportAfterRace(ctx, findingID)
	}
	if err != nil {
		return Report{}, fmt.Errorf("store verdict of finding %d: %w", findingID, err)
	}
	return t.toReport(stored)
}

func (t *Tools) reportAfterRace(ctx context.Context, findingID int64) (Report, error) {
	row, err := t.readReport(ctx, findingID)
	if err != nil {
		return Report{}, err
	}
	return t.toReport(row)
}

func encodeDecision(d decision) ([]byte, []byte, error) {
	observed, err := json.Marshal(d.observed)
	if err != nil {
		return nil, nil, fmt.Errorf("encode observed change: %w", err)
	}
	evidence, err := json.Marshal(d.evidence)
	if err != nil {
		return nil, nil, fmt.Errorf("encode verdict evidence: %w", err)
	}
	return observed, evidence, nil
}

// decide judges a deploy: unverifiable when the packet's index is not a
// valid index or there are no targets; otherwise the target queries'
// before/after comparison over the window, against the prediction.
func (t *Tools) decide(ctx context.Context, row reportRow) (decision, error) {
	var pred verify.Prediction
	if err := json.Unmarshal(row.Prediction, &pred); err != nil {
		return decision{}, fmt.Errorf("decode prediction of finding %d: %w", row.FindingID, err)
	}
	dep, w := *row.DeployedAt, t.opts.VerifyWindow
	d := decision{evidence: map[string]any{
		"before_window":   []time.Time{dep.Add(-w), dep},
		"after_window":    []time.Time{dep, dep.Add(w)},
		"target_queryids": toQueryIDs(row.Targets)}}
	reason, err := t.unverifiableReason(ctx, row)
	if err != nil {
		return decision{}, err
	}
	if reason != "" {
		d.verdict, d.reason = verify.OutcomeUnverifiable, reason
	} else if err := t.compare(ctx, row.Targets, dep, w, &d); err != nil {
		return decision{}, err
	}
	d.tolerance = verify.ToleranceVerdict(pred, d.verdict, d.observed.ChangePct)
	return d, nil
}

// unverifiableReason is why the deploy cannot be judged, or "".
func (t *Tools) unverifiableReason(ctx context.Context, row reportRow) (string, error) {
	if len(row.Targets) == 0 {
		return "the finding names no target queries to measure", nil
	}
	f, err := t.loadFinding(ctx, row.FindingID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Sprintf("finding %d no longer exists", row.FindingID), nil
	}
	if err != nil {
		return "", err
	}
	change, err := findingChange(f)
	if errors.Is(err, ErrNoChange) {
		return fmt.Sprintf("finding %d no longer carries a change", row.FindingID), nil
	}
	if err != nil {
		return "", err
	}
	index := createdIndex(change.Up)
	if index == nil {
		return "", nil
	}
	valid, err := t.indexValid(ctx, *index)
	if err != nil || valid {
		return "", err
	}
	return fmt.Sprintf("index %s from the packet is not a valid index in the catalog "+
		"after the deploy, so the deploy did not apply the packet", index), nil
}

const indexValidSQL = `/* pg_sage */ SELECT COALESCE((SELECT i.indisvalid FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relname = $2 AND (n.nspname = $1 OR ($1 = '' AND
        c.oid = to_regclass(quote_ident($2))))), false)`

func (t *Tools) indexValid(ctx context.Context, ref indexRef) (bool, error) {
	var valid bool
	if err := t.pool.QueryRow(ctx, indexValidSQL, ref.Schema, ref.Name).Scan(&valid); err != nil {
		return false, fmt.Errorf("check index %s: %w", ref, err)
	}
	return valid, nil
}

// compare measures the targets on both sides of the deploy.
func (t *Tools) compare(ctx context.Context, ids []int64, dep time.Time, w time.Duration,
	d *decision) error {
	src := verify.NewPostgresObservationSource(t.pool)
	before, err := src.QueryMeasurements(ctx, ids, dep.Add(-w), dep)
	if err != nil {
		return fmt.Errorf("measure before the deploy: %w", err)
	}
	after, err := src.QueryMeasurements(ctx, ids, dep, dep.Add(w))
	if err != nil {
		return fmt.Errorf("measure after the deploy: %w", err)
	}
	cmp, per := verify.DecideTargets(before, after, ids, verify.Thresholds{})
	d.verdict, d.reason = cmp.Verdict, cmp.Reason
	d.observed = verify.Observed{Metric: verify.MetricMeanExecTime,
		Before: durationMs(cmp.Before.AverageLatency), After: durationMs(cmp.After.AverageLatency)}
	switch cmp.Verdict {
	case verify.OutcomeImproved, verify.OutcomeNeutral, verify.OutcomeRegressed:
		pct := cmp.DeltaPct
		d.observed.ChangePct = &pct
	}
	d.evidence["comparison"] = cmp.Evidence()
	d.evidence["targets"] = verify.TargetsEvidence(per)
	return nil
}

func durationMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
