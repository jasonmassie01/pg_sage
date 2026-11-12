package value

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// linkedVerification joins the verification an action links to. Both
// verification writers repoint action_log.verification_id at the newest
// row, so this is the current verdict; an older success never masks a
// newer revert, and an unlinked row earns nothing (earned-09).
const linkedVerification = `LEFT JOIN sage.verification v
		ON v.id=al.verification_id AND v.action_log_id=al.id`

// CreditCandidate loads an action, its linked verification and its model.
func (r *PostgresRepository) CreditCandidate(
	ctx context.Context, actionID int64,
) (CreditCandidate, error) {
	var item CreditCandidate
	err := r.pool.QueryRow(ctx, `SELECT al.id, al.action_type, al.outcome,
		CASE WHEN v.verdict='success' AND v.completed_at IS NOT NULL
			THEN 'verified' ELSE COALESCE(v.verdict, 'missing') END
		FROM sage.action_log al
		`+linkedVerification+` WHERE al.id=$1`, actionID).Scan(
		&item.ActionID, &item.ActionType, &item.Outcome, &item.VerificationState)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditCandidate{}, fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return CreditCandidate{}, fmt.Errorf("load action credit candidate: %w", err)
	}
	item.ModelMinutes, item.ModelVersion, err = r.currentToilModel(ctx,
		ToilActionType(item.ActionType))
	if err != nil {
		return CreditCandidate{}, fmt.Errorf("load toil model of action %d: %w",
			actionID, err)
	}
	return item, nil
}

// currentToilModel reads the active model of a toil action type; an
// action type without one is priced 0 at version 0 (no credit).
func (r *PostgresRepository) currentToilModel(
	ctx context.Context, toilActionType string,
) (float64, int, error) {
	var minutes float64
	var version int
	err := r.pool.QueryRow(ctx, `SELECT base_minutes::float8, model_version
		FROM sage.toil_model WHERE action_type=$1 AND effective_to IS NULL`,
		toilActionType).Scan(&minutes, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return minutes, version, err
}

// StampCredit writes the credit once, only while the action succeeded and
// its linked verification is a completed success.
func (r *PostgresRepository) StampCredit(
	ctx context.Context, actionID int64, minutes float64, modelVersion int,
) error {
	tag, err := r.pool.Exec(ctx, `UPDATE sage.action_log al SET
		toil_minutes_saved=COALESCE(al.toil_minutes_saved, $2),
		toil_model_version=COALESCE(al.toil_model_version, $3)
		WHERE al.id=$1 AND al.outcome='success' AND EXISTS
		(SELECT 1 FROM sage.verification v WHERE v.id=al.verification_id
		 AND v.action_log_id=al.id AND v.verdict='success' AND v.completed_at IS NOT NULL)`,
		actionID, minutes, modelVersion)
	if err != nil {
		return fmt.Errorf("stamp action credit: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCreditNotEligible
	}
	return nil
}

func (r *PostgresRepository) ZeroCreditOnRevert(
	ctx context.Context, actionID int64, outcome string,
) (ZeroCreditResult, error) {
	return r.zeroCreditAtomic(ctx, actionID, outcome)
}

// zeroCreditAtomic zeroes the action's toil credit and, in the same
// statement, stamps reverted_at on the incident credits it earned (the
// rows stay as evidence; the report skips them).
func (r *PostgresRepository) zeroCreditAtomic(
	ctx context.Context, actionID int64, outcome string,
) (ZeroCreditResult, error) {
	var previous float64
	var applied bool
	err := r.pool.QueryRow(ctx, `WITH old AS (
		SELECT id, COALESCE(toil_minutes_saved,0)::float8 AS minutes
		FROM sage.action_log WHERE id=$1 FOR UPDATE), changed AS (
		UPDATE sage.action_log al SET toil_minutes_saved=0, outcome=$2
		FROM old WHERE al.id=old.id RETURNING old.minutes), voided AS (
		UPDATE sage.incident_avoided ia SET reverted_at=now()
		FROM old WHERE ia.action_log_id=old.id AND ia.reverted_at IS NULL
		RETURNING ia.id)
		SELECT minutes, minutes<>0 FROM changed`, actionID, outcome).Scan(&previous, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return ZeroCreditResult{}, fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return ZeroCreditResult{}, fmt.Errorf("zero action credit: %w", err)
	}
	return ZeroCreditResult{Applied: applied, PreviousMinutes: previous}, nil
}

// IncidentCandidate loads the decision and verification behind an action.
// The linked (latest) verification decides whether the action is verified.
func (r *PostgresRepository) IncidentCandidate(
	ctx context.Context, actionID int64,
) (IncidentCandidate, error) {
	item := IncidentCandidate{}
	err := r.pool.QueryRow(ctx, `SELECT al.id, al.database_id, al.outcome,
		COALESCE(v.decision_id, 0), COALESCE(v.id, 0),
		CASE WHEN v.verdict='success' AND v.completed_at IS NOT NULL
			THEN 'verified' ELSE COALESCE(v.verdict, 'missing') END
		FROM sage.action_log al
		`+linkedVerification+`
		WHERE al.id=$1`, actionID).Scan(
		&item.ActionID, &item.DatabaseID, &item.Outcome, &item.DecisionID,
		&item.VerificationID, &item.VerificationState)
	if errors.Is(err, pgx.ErrNoRows) {
		return IncidentCandidate{}, fmt.Errorf("action %d not found", actionID)
	}
	if err != nil {
		return IncidentCandidate{}, fmt.Errorf("load action incident candidate: %w", err)
	}
	return item, nil
}

// RecordIncident inserts one incident credit. The evidence id is unique:
// a second credit for the same evidence returns ErrIncidentAlreadyCredited.
func (r *PostgresRepository) RecordIncident(
	ctx context.Context, input IncidentCredit,
) (IncidentRecord, error) {
	var result IncidentRecord
	err := r.pool.QueryRow(ctx, `INSERT INTO sage.incident_avoided
		(database_id, kind, severity, credited_minutes, evidence_id, model_version,
		 decision_id, action_log_id, verification_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (evidence_id) DO NOTHING
		RETURNING id, credited_minutes::float8`, input.DatabaseID, input.Kind,
		input.Severity, input.CreditedMinutes, input.EvidenceID, input.ModelVersion,
		input.DecisionID, input.ActionLogID, input.VerificationID, input.OccurredAt).Scan(
		&result.ID, &result.CreditedMinutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return IncidentRecord{}, ErrIncidentAlreadyCredited
	}
	if err != nil {
		return IncidentRecord{}, fmt.Errorf("record incident credit: %w", err)
	}
	return result, nil
}
