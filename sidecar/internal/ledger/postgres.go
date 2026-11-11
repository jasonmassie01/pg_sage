package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// querier is what the repository runs statements on: a pool, or a
// transaction (NewTxRepository), where Begin opens a savepoint.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

type PostgresRepository struct{ pool querier }

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// NewTxRepository records inside tx, so a decision commits or rolls back
// with the transaction it was decided in (the standing gate's budget
// lock). A fingerprint upsert that fails there aborts the transaction:
// there is no plain-insert fallback inside it.
func NewTxRepository(tx pgx.Tx) *PostgresRepository {
	return &PostgresRepository{pool: tx}
}

func (r *PostgresRepository) InsertDecision(
	ctx context.Context, input DecisionInput,
) (int64, error) {
	values, err := decisionValues(input)
	if err != nil {
		return 0, err
	}
	statement := insertDecisionSQL
	if input.Verdict == VerdictExecute {
		statement = insertExecuteDecisionSQL
	}
	var id int64
	err = r.pool.QueryRow(ctx, statement, values...).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, fmt.Errorf("%w: %s", ErrEvidenceConflict, input.EvidenceID)
		}
		return 0, fmt.Errorf("persist decision: %w", err)
	}
	return id, nil
}

const insertDecisionColumns = `INSERT INTO sage.decision
	(database_id, feature, intent, target_objects, policy_version, verdict,
	 risk_tier, reason, evidence, evidence_id, deadline_kind, deadline_hard_at,
	 fingerprint, principal_id, task_id, artifact_hash, candidate_key,
	 guardrails, off_window_ok, policy_doc_sha256)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12,$13,
	        NULLIF($14,''),NULLIF($15,''),NULLIF($16,''),$17,$18,$19,$20)`

const insertDecisionSQL = insertDecisionColumns + ` RETURNING id`

// insertExecuteDecisionSQL records an execute verdict and, in the same
// statement, resolves the candidate's open withheld (fingerprinted) rows:
// the episode they counted is over, and the next withheld verdict starts
// a new row (policy-02). One statement, so the execute is never recorded
// without the resolution, inside a transaction or not.
const insertExecuteDecisionSQL = `WITH resolved AS (
	UPDATE sage.decision SET resolved_at = now()
	 WHERE candidate_key = $17 AND resolved_at IS NULL AND fingerprint IS NOT NULL)
	` + insertDecisionColumns + ` RETURNING id`

// LinkQueue records which queue item a decision queued and resolves the
// decision: the queue item owns that verdict, and the gate's next equal
// verdict is a new row for the next queue item (policy-02). A decision
// already linked keeps its link. It reports whether the link was made.
func (r *PostgresRepository) LinkQueue(
	ctx context.Context, decisionID int64, queueID int,
) (bool, error) {
	if decisionID <= 0 || queueID <= 0 {
		return false, nil
	}
	tag, err := r.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.decision
		SET queue_id = $2, resolved_at = COALESCE(resolved_at, now())
		WHERE id = $1 AND queue_id IS NULL`, decisionID, queueID)
	if err != nil {
		return false, fmt.Errorf("link decision %d to queue item %d: %w",
			decisionID, queueID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// decisionValues encodes a decision as the decisionParamCount INSERT
// parameters. An insert that is not an upsert stores no fingerprint, so it
// can never collide with an open fingerprinted row. Every row carries its
// candidate key and its policy provenance (policy-03).
func decisionValues(input DecisionInput) ([]any, error) {
	targets, err := json.Marshal(input.TargetObjects)
	if err != nil {
		return nil, fmt.Errorf("persist decision targets: %w", err)
	}
	guardrails, err := json.Marshal(nonNilStrings(input.Guardrails))
	if err != nil {
		return nil, fmt.Errorf("persist decision guardrails: %w", err)
	}
	evidence := cloneEvidence(input.Evidence)
	if input.ProposedSQL != "" {
		evidence["proposed_sql"] = input.ProposedSQL
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return nil, fmt.Errorf("persist decision evidence: %w", err)
	}
	return []any{input.DatabaseID, input.Feature, input.Intent, targets,
		input.PolicyVersion, input.Verdict, input.RiskTier, input.Reason, evidenceJSON,
		input.EvidenceID, input.DeadlineKind, input.DeadlineHardAt, nil,
		input.PrincipalID, input.TaskID, input.ArtifactHash, CandidateKey(input),
		guardrails, input.OffWindowOK, nilIfEmpty(input.PolicySHA256)}, nil
}

// The decisionValues parameters the provenance tests and the upsert name.
const (
	guardrailsParam    = 17 // $18
	offWindowParam     = 18 // $19
	policyHashParam    = 19 // $20
	decisionParamCount = 20
)

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// nilIfEmpty stores "" as NULL: a decision that read no policy has no hash.
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// FindAuditViolations reports recent actions that lack a decision or a
// verification. Rows still being verified are not yet violations, and the
// audit covers a rolling 24-hour window so historic rows are reported once
// rather than on every tick forever.
func (r *PostgresRepository) FindAuditViolations(
	ctx context.Context,
) ([]AuditViolation, error) {
	rows, err := r.pool.Query(ctx, `SELECT al.id,
		CASE WHEN al.decision_id IS NULL THEN 'missing_decision'
			ELSE 'missing_verification' END
		FROM sage.action_log al
		LEFT JOIN sage.verification v ON v.action_log_id=al.id
		WHERE al.outcome NOT IN ('reverted', 'rolled_back', 'monitoring', 'pending',
		                         'interrupted', 'rolling_back')
		AND al.executed_at > now() - interval '24 hours'
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
