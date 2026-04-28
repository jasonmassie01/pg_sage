package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/migration"
)

type MigrationSafetyFindingStore struct {
	pool *pgxpool.Pool
}

func NewMigrationSafetyFindingStore(
	pool *pgxpool.Pool,
) *MigrationSafetyFindingStore {
	return &MigrationSafetyFindingStore{pool: pool}
}

func (s *MigrationSafetyFindingStore) UpsertMigrationSafetyFinding(
	ctx context.Context,
	input migration.MigrationSafetyFinding,
) (int64, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	detail, err := json.Marshal(input.Detail)
	if err != nil {
		return 0, fmt.Errorf("encoding migration safety finding: %w", err)
	}
	id, found, err := s.openMigrationFindingID(qctx, input)
	if err != nil {
		return 0, err
	}
	if found {
		return id, s.updateMigrationFinding(qctx, id, input, detail)
	}
	return s.insertMigrationFinding(qctx, input, detail)
}

func (s *MigrationSafetyFindingStore) openMigrationFindingID(
	ctx context.Context,
	input migration.MigrationSafetyFinding,
) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM sage.findings
		  WHERE category = $1
		    AND object_identifier = $2
		    AND status = 'open'`,
		migration.SafetyFindingCategory, input.ObjectIdentifier,
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("lookup migration safety finding: %w", err)
}

func (s *MigrationSafetyFindingStore) updateMigrationFinding(
	ctx context.Context,
	id int64,
	input migration.MigrationSafetyFinding,
	detail []byte,
) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sage.findings
		    SET last_seen = now(),
		        occurrence_count = occurrence_count + 1,
		        severity = $2,
		        title = $3,
		        detail = $4,
		        recommendation = $5,
		        recommended_sql = $6,
		        rule_id = $7,
		        impact_score = $8
		  WHERE id = $1`,
		id, input.Severity, input.Title, detail,
		input.Recommendation, input.RecommendedSQL,
		input.RuleID, nullableImpact(input.ImpactScore),
	)
	if err != nil {
		return fmt.Errorf("update migration safety finding: %w", err)
	}
	return nil
}

func (s *MigrationSafetyFindingStore) insertMigrationFinding(
	ctx context.Context,
	input migration.MigrationSafetyFinding,
	detail []byte,
) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		    (category, severity, object_type, object_identifier,
		     title, detail, recommendation, recommended_sql,
		     rollback_sql, status, last_seen, occurrence_count,
		     rule_id, impact_score)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'','open',now(),1,$9,$10)
		 RETURNING id`,
		migration.SafetyFindingCategory, input.Severity,
		input.ObjectType, input.ObjectIdentifier, input.Title, detail,
		input.Recommendation, input.RecommendedSQL,
		input.RuleID, nullableImpact(input.ImpactScore),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert migration safety finding: %w", err)
	}
	return id, nil
}

func nullableImpact(score float64) any {
	if score == 0 {
		return nil
	}
	return score
}
