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
//
// The llm_ keys of the detail are the enricher's model output, written
// after the detection (migration-10): a re-detection that carries none
// keeps the open row's, and one that carries any replaces them all.
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
               detail = CASE
                   WHEN EXISTS (SELECT 1 FROM jsonb_object_keys(EXCLUDED.detail) AS k
                                 WHERE k LIKE 'llm\_%')
                   THEN EXCLUDED.detail
                   ELSE COALESCE((SELECT jsonb_object_agg(e.key, e.value)
                                    FROM jsonb_each(sage.findings.detail) AS e
                                   WHERE e.key LIKE 'llm\_%'), '{}'::jsonb)
                        || EXCLUDED.detail
               END,
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

// mergeMigrationLLMDetailSQL replaces the llm_ keys of the open finding's
// detail with $3; a finding no longer open is left alone (migration-10).
const mergeMigrationLLMDetailSQL = `/* pg_sage */ UPDATE sage.findings
    SET detail = COALESCE((SELECT jsonb_object_agg(e.key, e.value)
                             FROM jsonb_each(detail) AS e
                            WHERE e.key NOT LIKE 'llm\_%'), '{}'::jsonb) || $3::jsonb
  WHERE category = $1 AND object_identifier = $2 AND status = 'open'`

// MergeMigrationFindingLLMDetail records the enricher's model output on
// the open finding identity names, replacing any earlier model output.
func (s *MigrationSafetyFindingStore) MergeMigrationFindingLLMDetail(
	ctx context.Context, identity string, fields map[string]any,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("record model output for %q: store has no database pool", identity)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encoding model output for %q: %w", identity, err)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(qctx, mergeMigrationLLMDetailSQL,
		migration.SafetyFindingCategory, identity, raw); err != nil {
		return fmt.Errorf("record model output for %q: %w", identity, err)
	}
	return nil
}

func nullableImpact(score float64) any {
	if score == 0 {
		return nil
	}
	return score
}
