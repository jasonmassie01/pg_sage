package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/ledger"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
)

type PostgresApplier struct {
	pool             *pgxpool.Pool
	lockTimeout      time.Duration
	statementTimeout time.Duration
}

func NewPostgresApplier(
	pool *pgxpool.Pool, lockTimeout, statementTimeout time.Duration,
) *PostgresApplier {
	return &PostgresApplier{pool, lockTimeout, statementTimeout}
}

func (a *PostgresApplier) Apply(ctx context.Context, step planpkg.Step) error {
	if a == nil || a.pool == nil {
		return fmt.Errorf("migration apply pool is unavailable")
	}
	lockTimeoutMS := int(a.lockTimeout / time.Millisecond)
	option := executor.WithLockTimeout(lockTimeoutMS)
	var err error
	if step.RequiresTopLevel {
		err = executor.ExecConcurrently(
			ctx, a.pool, step.SQL, a.statementTimeout, option,
		)
	} else {
		err = executor.ExecInTransaction(
			ctx, a.pool, step.SQL, a.statementTimeout, option,
		)
	}
	if err != nil {
		return fmt.Errorf("apply migration step %s: %w", step.Kind, err)
	}
	return nil
}

type PostgresRecorder struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewPostgresRecorder(pool *pgxpool.Pool) *PostgresRecorder {
	return &PostgresRecorder{pool: pool, now: time.Now}
}

func (r *PostgresRecorder) Record(ctx context.Context, record Record) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("migration record pool is unavailable")
	}
	evidenceID := record.EvidenceID
	if evidenceID == "" {
		evidenceID = ledger.NewEvidenceID()
	}
	contractAt := contractTime(r.now(), record)
	measurement, err := measurementJSON(record)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO sage.migration_run
		(database_id, evidence_id, phase, source_sql_hash, measurement, verdict,
		 contract_not_before)
		VALUES ($1, $2, 'expand', $3, $6::jsonb, $4, $5)
		ON CONFLICT (evidence_id) DO UPDATE SET
		database_id=EXCLUDED.database_id, phase=EXCLUDED.phase,
		measurement=EXCLUDED.measurement,
		verdict=EXCLUDED.verdict, contract_not_before=EXCLUDED.contract_not_before,
		updated_at=now()`, record.Request.DatabaseID, evidenceID,
		migrationHash(record.Request.SQL),
		databaseMigrationVerdict(record.Verdict), contractAt, measurement)
	if err != nil {
		return fmt.Errorf("persist migration continuation: %w", err)
	}
	return nil
}

// measurementJSON persists the rehearsal evidence and the reason (which
// carries clone-cleanup failures) instead of an empty object (G7-B22).
func measurementJSON(record Record) ([]byte, error) {
	doc := map[string]any{"reason": record.Reason}
	if m := record.Measurement; m != nil {
		doc["max_lock_duration_ms"] = float64(m.MaxLockDuration.Microseconds()) / 1000
		doc["backfill_duration_ms"] = float64(m.BackfillDuration.Microseconds()) / 1000
		doc["disk_delta_bytes"] = m.DiskDeltaBytes
		doc["affected_queries"] = affectedQueriesJSON(m.AffectedQueries)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode migration measurement: %w", err)
	}
	return raw, nil
}

func affectedQueriesJSON(queries []rehearsalpkg.QueryMeasurement) []map[string]any {
	out := make([]map[string]any, 0, len(queries))
	for _, q := range queries {
		out = append(out, map[string]any{
			"query_id": q.QueryID, "before_latency_ms": q.BeforeLatencyMS,
			"after_latency_ms": q.AfterLatencyMS, "before_plan_hash": q.BeforePlanHash,
			"after_plan_hash": q.AfterPlanHash,
		})
	}
	return out
}

func contractTime(now time.Time, record Record) *time.Time {
	if record.ContractNotBeforeCycle <= record.Request.Cycle {
		return nil
	}
	delta := record.ContractNotBeforeCycle - record.Request.Cycle
	result := now.Add(time.Duration(delta) * time.Minute)
	return &result
}

func migrationHash(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

func databaseMigrationVerdict(verdict Verdict) string {
	switch verdict {
	case VerdictExpanded:
		return "promote"
	case VerdictRecommendOnly:
		return "recommend_only"
	case VerdictParked:
		return "parked"
	default:
		return "failed"
	}
}

var _ Applier = (*PostgresApplier)(nil)
var _ Recorder = (*PostgresRecorder)(nil)
