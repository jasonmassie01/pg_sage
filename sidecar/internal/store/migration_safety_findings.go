package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

// upsertMigrationFindingSQL records one detection atomically against the
// partial unique index idx_findings_dedup (category, object_identifier)
// WHERE status = 'open'. Two detectors seeing the same DDL at once both
// count; neither fails with 23505 (migration-12).
const upsertMigrationFindingSQL = `/* pg_sage */ INSERT INTO sage.findings
    (category, severity, object_type, object_identifier,
     title, detail, recommendation, recommended_sql,
     rollback_sql, status, last_seen, occurrence_count,
     rule_id, impact_score)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'','open',now(),1,$9,$10)
 ON CONFLICT (category, object_identifier) WHERE status = 'open'
 DO UPDATE SET last_seen = now(),
               occurrence_count = sage.findings.occurrence_count + 1,
               severity = EXCLUDED.severity,
               title = EXCLUDED.title,
               detail = EXCLUDED.detail,
               recommendation = EXCLUDED.recommendation,
               recommended_sql = EXCLUDED.recommended_sql,
               rule_id = EXCLUDED.rule_id,
               impact_score = EXCLUDED.impact_score
 RETURNING id`

// UpsertMigrationSafetyFinding opens the finding or counts one more
// occurrence of the open one, and returns its id.
func (s *MigrationSafetyFindingStore) UpsertMigrationSafetyFinding(
	ctx context.Context,
	input migration.MigrationSafetyFinding,
) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("upsert migration safety finding %q: store has no database pool",
			input.ObjectIdentifier)
	}
	detail, err := json.Marshal(input.Detail)
	if err != nil {
		return 0, fmt.Errorf("encoding migration safety finding: %w", err)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var id int64
	err = s.pool.QueryRow(qctx, upsertMigrationFindingSQL,
		migration.SafetyFindingCategory, input.Severity,
		input.ObjectType, input.ObjectIdentifier, input.Title, detail,
		input.Recommendation, input.RecommendedSQL,
		input.RuleID, nullableImpact(input.ImpactScore),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert migration safety finding %q: %w",
			input.ObjectIdentifier, err)
	}
	return id, nil
}

func nullableImpact(score float64) any {
	if score == 0 {
		return nil
	}
	return score
}
