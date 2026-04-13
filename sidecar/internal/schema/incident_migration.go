package schema

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateIncidentConstraints widens the CHECK constraints on
// sage.incidents for v0.9.1 log-based RCA sources, info severity,
// and Tier 2 action_risk values. Idempotent — safe to re-run.
func migrateIncidentConstraints(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	const ddl = `
DO $$ BEGIN
    -- severity: add 'info'
    IF EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_schema = 'sage'
          AND table_name   = 'incidents'
          AND constraint_type = 'CHECK'
          AND constraint_name = 'incidents_severity_check'
    ) THEN
        ALTER TABLE sage.incidents DROP CONSTRAINT incidents_severity_check;
        ALTER TABLE sage.incidents
            ADD CONSTRAINT incidents_severity_check
            CHECK (severity IN ('info', 'warning', 'critical'));
    END IF;

    -- source: add log_deterministic, self_action, manual_review_required
    IF EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_schema = 'sage'
          AND table_name   = 'incidents'
          AND constraint_type = 'CHECK'
          AND constraint_name = 'incidents_source_check'
    ) THEN
        ALTER TABLE sage.incidents DROP CONSTRAINT incidents_source_check;
        ALTER TABLE sage.incidents
            ADD CONSTRAINT incidents_source_check
            CHECK (source IN (
                'deterministic', 'log_deterministic',
                'self_action', 'manual_review_required', 'llm'
            ));
    END IF;

    -- action_risk: add low, medium, high (Tier 2 values)
    IF EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_schema = 'sage'
          AND table_name   = 'incidents'
          AND constraint_type = 'CHECK'
          AND constraint_name = 'incidents_action_risk_check'
    ) THEN
        ALTER TABLE sage.incidents DROP CONSTRAINT incidents_action_risk_check;
        ALTER TABLE sage.incidents
            ADD CONSTRAINT incidents_action_risk_check
            CHECK (action_risk IN (
                'safe', 'moderate', 'high_risk',
                'low', 'medium', 'high'
            ) OR action_risk IS NULL);
    END IF;
END $$;`

	_, err := pool.Exec(ctx, ddl)
	return err
}
