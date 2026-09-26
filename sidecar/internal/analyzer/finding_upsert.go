package analyzer

import (
	"context"
	"encoding/json"
	"errors"

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

type upsertOutcome int

const (
	outcomeSkipped upsertOutcome = iota
	outcomeUpdated
	outcomeEscalated
	outcomeOpened
	outcomeSuppressed
)

// UpsertFindings persists a batch of findings, incrementing occurrence_count
// for existing open findings and inserting new ones.
func UpsertFindings(ctx context.Context, pool *pgxpool.Pool, findings []Finding) error {
	_, err := UpsertFindingsWithResult(ctx, pool, findings)
	return err
}

// UpsertFindingsWithResult persists findings and reports which were
// opened, escalated or suppressed. An active suppression (status
// 'suppressed' with suppressed_until NULL or in the future) on the same
// identity is honoured: no open row is created or refreshed for it.
func UpsertFindingsWithResult(
	ctx context.Context, pool *pgxpool.Pool, findings []Finding,
) (UpsertResult, error) {
	var res UpsertResult
	for _, f := range findings {
		if isSelfMonitoringFinding(f) {
			continue
		}
		outcome, err := upsertOne(ctx, pool, f)
		if err != nil {
			return res, err
		}
		switch outcome {
		case outcomeOpened:
			res.Opened = append(res.Opened, f)
		case outcomeEscalated:
			res.Escalated = append(res.Escalated, f)
		case outcomeSuppressed:
			res.Suppressed = append(res.Suppressed, f)
		}
	}
	return res, nil
}

func upsertOne(
	ctx context.Context, pool *pgxpool.Pool, f Finding,
) (upsertOutcome, error) {
	suppressed, err := suppressionActive(ctx, pool, f)
	if err != nil {
		return outcomeSkipped, err
	}
	if suppressed {
		return outcomeSuppressed, nil
	}
	detailJSON, err := json.Marshal(f.Detail)
	if err != nil {
		return outcomeSkipped, err
	}
	var existingID, existingSeverity string
	err = pool.QueryRow(ctx,
		`/* pg_sage */ SELECT id, severity FROM sage.findings
		 WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
		f.Category, f.ObjectIdentifier,
	).Scan(&existingID, &existingSeverity)
	if err == nil {
		if err := refreshOpenFinding(ctx, pool, existingID, f, detailJSON); err != nil {
			return outcomeSkipped, err
		}
		if severityRank(f.Severity) > severityRank(existingSeverity) {
			return outcomeEscalated, nil
		}
		return outcomeUpdated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return outcomeSkipped, err
	}
	recentlyResolved, err := recentlyResolvedByAction(
		ctx, pool, f.Category, f.ObjectIdentifier)
	if err != nil || recentlyResolved {
		return outcomeSkipped, err
	}
	if err := insertFinding(ctx, pool, f, detailJSON); err != nil {
		return outcomeSkipped, err
	}
	return outcomeOpened, nil
}

// suppressionActive reports whether an operator suppression currently
// covers f's identity. suppressed_until NULL means "until unsuppressed".
func suppressionActive(
	ctx context.Context, pool *pgxpool.Pool, f Finding,
) (bool, error) {
	var active bool
	err := pool.QueryRow(ctx,
		`/* pg_sage */ SELECT EXISTS (
		    SELECT 1 FROM sage.findings
		     WHERE category = $1 AND object_identifier = $2
		       AND status = 'suppressed'
		       AND (suppressed_until IS NULL OR suppressed_until > now()))`,
		f.Category, f.ObjectIdentifier,
	).Scan(&active)
	return active, err
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
		`/* pg_sage */ UPDATE sage.findings
		 SET last_seen = now(),
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
		         ELSE impact_score END
		 WHERE id = $9`,
		detailJSON, f.Severity, f.Title,
		f.Recommendation, f.RecommendedSQL, f.RollbackSQL,
		f.RuleID, f.ImpactScore, id,
	)
	return err
}

// insertFinding inserts a new open finding. rule_id/impact_score are
// NULL when unset so non-lint findings don't populate these columns.
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
	_, err := pool.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, recommended_sql,
		  rollback_sql, status, last_seen, occurrence_count,
		  rule_id, impact_score)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',now(),1,
		         $10,$11)`,
		f.Category, f.Severity, f.ObjectType, f.ObjectIdentifier,
		f.Title, detailJSON, f.Recommendation, f.RecommendedSQL,
		f.RollbackSQL, ruleID, impactScore,
	)
	return err
}
