package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct{ pool *pgxpool.Pool }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) InsertDecision(
	ctx context.Context, input DecisionInput,
) (int64, error) {
	targets, err := json.Marshal(input.TargetObjects)
	if err != nil {
		return 0, fmt.Errorf("persist decision targets: %w", err)
	}
	evidence := cloneEvidence(input.Evidence)
	if input.ProposedSQL != "" {
		evidence["proposed_sql"] = input.ProposedSQL
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return 0, fmt.Errorf("persist decision evidence: %w", err)
	}
	var id int64
	err = r.pool.QueryRow(ctx, `INSERT INTO sage.decision
		(database_id, feature, intent, target_objects, policy_version, verdict,
		 risk_tier, reason, evidence, evidence_id, deadline_kind, deadline_hard_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12) RETURNING id`,
		input.DatabaseID, input.Feature, input.Intent, targets, input.PolicyVersion,
		input.Verdict, input.RiskTier, input.Reason, evidenceJSON, input.EvidenceID,
		input.DeadlineKind, input.DeadlineHardAt).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, fmt.Errorf("%w: %s", ErrEvidenceConflict, input.EvidenceID)
		}
		return 0, fmt.Errorf("persist decision: %w", err)
	}
	return id, nil
}

func (r *PostgresRepository) FindAuditViolations(
	ctx context.Context,
) ([]AuditViolation, error) {
	rows, err := r.pool.Query(ctx, `SELECT al.id,
		CASE WHEN al.decision_id IS NULL THEN 'missing_decision'
			ELSE 'missing_verification' END
		FROM sage.action_log al
		LEFT JOIN sage.verification v ON v.action_log_id=al.id
		WHERE al.outcome NOT IN ('reverted','rolled_back')
		AND (al.decision_id IS NULL OR v.id IS NULL) ORDER BY al.id`)
	if err != nil {
		return nil, fmt.Errorf("run ledger self-audit: %w", err)
	}
	defer rows.Close()
	result := make([]AuditViolation, 0)
	for rows.Next() {
		var item AuditViolation
		if err := rows.Scan(&item.ActionID, &item.Kind); err != nil {
			return nil, fmt.Errorf("scan ledger self-audit: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ledger self-audit: %w", err)
	}
	return result, nil
}

func cloneEvidence(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}
