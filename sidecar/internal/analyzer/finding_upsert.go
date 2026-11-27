package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpsertResult reports what UpsertFindingsWithResult did with each
// finding so callers can notify only on genuinely new information.
type UpsertResult struct {
	// Opened holds findings inserted as a new open row this call.
	Opened []Finding
	// Escalated holds existing open findings whose severity increased.
	Escalated []Finding
	// Suppressed holds findings skipped because an operator suppression
	// is active for their (category, object_identifier) identity.
	Suppressed []Finding
	// Reopened holds findings whose condition persisted after an action
	// resolved them: the action's row was reopened with
	// DetailPersistedAfterAction instead of a new row being opened.
	Reopened []Finding
	// Failed holds findings that could not be persisted this call.
	Failed []Finding
}

// IsSuppressed reports whether f's identity was suppressed.
func (r UpsertResult) IsSuppressed(f Finding) bool {
	for _, s := range r.Suppressed {
		if s.Category == f.Category && s.ObjectIdentifier == f.ObjectIdentifier {
			return true
		}
	}
	return false
}

func (r *UpsertResult) record(outcome upsertOutcome, f Finding) {
	switch outcome {
	case outcomeOpened:
		r.Opened = append(r.Opened, f)
	case outcomeEscalated:
		r.Escalated = append(r.Escalated, f)
	case outcomeSuppressed:
		r.Suppressed = append(r.Suppressed, f)
	case outcomeReopened:
		r.Reopened = append(r.Reopened, f)
	}
}

type upsertOutcome int

const (
	outcomeSkipped upsertOutcome = iota
	outcomeUpdated
	outcomeEscalated
	outcomeOpened
	outcomeSuppressed
	outcomeReopened
)

// UpsertFindings persists a batch of findings, incrementing occurrence_count
// for existing open findings and inserting new ones.
func UpsertFindings(ctx context.Context, pool *pgxpool.Pool, findings []Finding) error {
	_, err := UpsertFindingsWithResult(ctx, pool, findings)
	return err
}

// UpsertSystemFindings persists findings pg_sage raises about its own
// machinery (its audit trail, its SIEM export) like UpsertFindings, but
// without the workload filter: they name pg_sage's sinks and sources by
// design, and that filter would drop one whose name holds "pg_sage".
func UpsertSystemFindings(ctx context.Context, pool *pgxpool.Pool,
	findings []Finding) error {
	for _, f := range findings {
		if _, err := upsertOne(ctx, pool, f, Evidence{}); err != nil {
			return err
		}
	}
	return nil
}

// UpsertFindingsWithResult is UpsertFindingsObserved with an unknown
// evidence time, assumed unknownEvidenceAge old: a finding an action
// resolved more recently than that is treated as stale.
func UpsertFindingsWithResult(
	ctx context.Context, pool *pgxpool.Pool, findings []Finding,
) (UpsertResult, error) {
	return UpsertFindingsObserved(ctx, pool, findings, Evidence{})
}

// UpsertFindingsObserved persists findings observed at ev.CollectedAt and
// reports which were opened, escalated, reopened or suppressed. An active
// suppression (status 'suppressed' with suppressed_until NULL or in the
// future) on the same identity is honoured: no open row is created or
// refreshed for it. A per-finding error does not stop the batch: the
// finding goes to Failed and the returned error joins every failure,
// each naming its category and identity (rules-06). A lost connection or
// a cancelled ctx stops the batch: the rest go to Failed unattempted.
func UpsertFindingsObserved(
	ctx context.Context, pool *pgxpool.Pool, findings []Finding, ev Evidence,
) (UpsertResult, error) {
	var res UpsertResult
	var errs []error
	for i, f := range findings {
		if excludedFromAdvice(f) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, stopBatch(&res, errs, findings[i:], err)
		}
		outcome, err := upsertOne(ctx, pool, f, ev)
		if err == nil {
			res.record(outcome, f)
			continue
		}
		res.Failed = append(res.Failed, f)
		errs = append(errs, fmt.Errorf("upsert %s finding %s: %w",
			f.Category, f.ObjectIdentifier, err))
		if connectionLost(err) {
			return res, stopBatch(&res, errs, findings[i+1:], err)
		}
	}
	return res, errors.Join(errs...)
}

// stopBatch records the unattempted rest of a batch as failed and returns
// the batch's joined error, ending with why it stopped.
func stopBatch(res *UpsertResult, errs []error, rest []Finding, cause error) error {
	left := advisable(rest)
	res.Failed = append(res.Failed, left...)
	return errors.Join(append(errs,
		fmt.Errorf("upsert stopped with %d findings left: %w", len(left), cause))...)
}

// upsertOne persists f, retrying once when a concurrent upserter opened
// the same identity between this call's read and its write.
func upsertOne(
	ctx context.Context, pool *pgxpool.Pool, f Finding, ev Evidence,
) (upsertOutcome, error) {
	outcome, err := upsertAttempt(ctx, pool, f, ev)
	if errors.Is(err, errUpsertRaced) {
		outcome, err = upsertAttempt(ctx, pool, f, ev)
	}
	return outcome, err
}

func upsertAttempt(
	ctx context.Context, pool *pgxpool.Pool, f Finding, ev Evidence,
) (upsertOutcome, error) {
	suppressed, err := suppressionActive(ctx, pool, f)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("check suppression: %w", err)
	}
	if suppressed {
		return outcomeSuppressed, nil
	}
	open, err := lookupOpenFinding(ctx, pool, f)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("look up open row: %w", err)
	}
	if !open.found {
		return openOrReopen(ctx, pool, f, ev)
	}
	detailJSON, err := openDetailJSON(f, open)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("marshal detail: %w", err)
	}
	if err := refreshOpenFinding(ctx, pool, open.id, f, detailJSON); err != nil {
		return outcomeSkipped, fmt.Errorf("refresh open row %s: %w", open.id, err)
	}
	if severityRank(f.Severity) > severityRank(open.severity) {
		return outcomeEscalated, nil
	}
	return outcomeUpdated, nil
}

// openRow is the open finding row of an identity, if any.
type openRow struct {
	found    bool
	id       string
	severity string
	// persistedAfter is the action the row was reopened after, or 0.
	persistedAfter int64
}

func lookupOpenFinding(
	ctx context.Context, pool *pgxpool.Pool, f Finding,
) (openRow, error) {
	var r openRow
	var persisted *int64
	err := pool.QueryRow(ctx,
		`/* pg_sage analyzer */ SELECT id, severity,
		        CASE WHEN jsonb_typeof(detail->'persisted_after_action') = 'number'
		              AND detail->>'persisted_after_action' ~ '^[0-9]{1,18}$'
		             THEN (detail->>'persisted_after_action')::bigint END
		   FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
		f.Category, f.ObjectIdentifier,
	).Scan(&r.id, &r.severity, &persisted)
	if errors.Is(err, pgx.ErrNoRows) {
		return openRow{}, nil
	}
	if err != nil {
		return openRow{}, err
	}
	r.found = true
	if persisted != nil {
		r.persistedAfter = *persisted
	}
	return r, nil
}

// openDetailJSON is the refreshed detail of an open row. A row reopened
// after an action keeps its DetailPersistedAfterAction link while it stays
// open, since producers do not emit it.
func openDetailJSON(f Finding, open openRow) ([]byte, error) {
	if _, ok := f.Detail[DetailPersistedAfterAction]; open.persistedAfter == 0 || ok {
		return findingDetailJSON(f)
	}
	return detailWithPersistedAfterAction(f, open.persistedAfter)
}

// suppressionActive reports whether an operator suppression currently
// covers f's identity. suppressed_until NULL means "until unsuppressed".
func suppressionActive(
	ctx context.Context, pool *pgxpool.Pool, f Finding,
) (bool, error) {
	var active bool
	err := pool.QueryRow(ctx,
		`/* pg_sage analyzer */ SELECT EXISTS (
		    SELECT 1 FROM sage.findings
		     WHERE category = $1 AND object_identifier = $2
		       AND status = 'suppressed'
		       AND (suppressed_until IS NULL OR suppressed_until > now()))`,
		f.Category, f.ObjectIdentifier,
	).Scan(&active)
	return active, err
}

// findingRefreshSet is the SET list shared by refreshing an open row and
// reopening an action-resolved one: $1 detail, $2 severity, $3 title,
// $4 recommendation, $5 recommended_sql, $6 rollback_sql, $7 rule_id,
// $8 impact_score.
const findingRefreshSet = `last_seen = now(),
		     occurrence_count = occurrence_count + 1,
		     detail = $1,
		     severity = $2,
		     title = $3,
		     recommendation = $4,
		     recommended_sql = $5,
		     rollback_sql = $6,
		     rule_id = COALESCE(NULLIF($7, ''), rule_id),
		     impact_score = CASE
		         WHEN $8 <> 0 THEN $8::real
		         ELSE impact_score END`

// refreshArgs are findingRefreshSet's arguments for f.
func refreshArgs(f Finding, detailJSON []byte) []any {
	return []any{detailJSON, f.Severity, f.Title, f.Recommendation,
		f.RecommendedSQL, f.RollbackSQL, f.RuleID, f.ImpactScore}
}

// refreshOpenFinding bumps the count and refreshes every user-visible
// field. The forward and inverse SQL are always written together so a
// refreshed proposal never pairs new forward SQL with a stale rollback
// (C04). rule_id / impact_score are only refreshed when supplied.
func refreshOpenFinding(
	ctx context.Context, pool *pgxpool.Pool,
	id string, f Finding, detailJSON []byte,
) error {
	_, err := pool.Exec(ctx,
		`/* pg_sage analyzer */ UPDATE sage.findings SET `+findingRefreshSet+`
		 WHERE id = $9`,
		append(refreshArgs(f, detailJSON), id)...,
	)
	return err
}

// insertFinding inserts a new open finding. rule_id/impact_score are
// NULL when unset so non-lint findings don't populate these columns. A
// concurrent upserter that opened the identity first (idx_findings_dedup)
// yields errUpsertRaced so the caller refreshes that row instead.
func insertFinding(
	ctx context.Context, pool *pgxpool.Pool, f Finding, detailJSON []byte,
) error {
	var ruleID any
	if f.RuleID != "" {
		ruleID = f.RuleID
	}
	var impactScore any
	if f.ImpactScore != 0 {
		impactScore = f.ImpactScore
	}
	tag, err := pool.Exec(ctx,
		`/* pg_sage analyzer */ INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, recommended_sql,
		  rollback_sql, status, last_seen, occurrence_count,
		  rule_id, impact_score)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',now(),1,
		         $10,$11)
		 ON CONFLICT (category, object_identifier) WHERE status = 'open'
		 DO NOTHING`,
		f.Category, f.Severity, f.ObjectType, f.ObjectIdentifier,
		f.Title, detailJSON, f.Recommendation, f.RecommendedSQL,
		f.RollbackSQL, ruleID, impactScore,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errUpsertRaced
	}
	return nil
}

// findingDetailJSON is the stored detail: the producer's Detail plus its
// ActionRisk under DetailActionRisk (the field wins over a stale key). The
// caller's map is copied, never written.
func findingDetailJSON(f Finding) ([]byte, error) {
	if f.ActionRisk == "" {
		return json.Marshal(f.Detail)
	}
	detail := make(map[string]any, len(f.Detail)+1)
	for k, v := range f.Detail {
		detail[k] = v
	}
	detail[DetailActionRisk] = f.ActionRisk
	return json.Marshal(detail)
}
