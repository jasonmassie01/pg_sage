package schema

import (
	"context"
	"fmt"
)

// migrateIncidentConstraints widens the CHECK constraints on
// sage.incidents for v0.9.1 log-based RCA sources, info severity,
// and Tier 2 action_risk values. Idempotent — safe to re-run.
func migrateIncidentConstraints(
	ctx context.Context, db bootstrapDB,
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
                'self_action', 'manual_review_required', 'llm',
                'schema_advisor', 'schema_lint', 'n_plus_one'
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

	if _, err := db.Exec(ctx, ddl); err != nil {
		return err
	}
	return migrateIncidentLifecycle(ctx, db)
}

// ddlIncidentLifecycle adds the durable incident lifecycle columns
// (R04, SURF-19): who resolved an incident and why, the engine's identity
// key, and the link from a recurrence to the incident it follows.
// Additive and idempotent.
const ddlIncidentLifecycle = `
ALTER TABLE sage.incidents
    ADD COLUMN IF NOT EXISTS resolved_by          TEXT,
    ADD COLUMN IF NOT EXISTS resolution_reason    TEXT,
    ADD COLUMN IF NOT EXISTS identity_key         TEXT,
    ADD COLUMN IF NOT EXISTS previous_incident_id UUID;
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'incidents_previous_incident_fk'
          AND conrelid = 'sage.incidents'::regclass
    ) THEN
        ALTER TABLE sage.incidents
            ADD CONSTRAINT incidents_previous_incident_fk
            FOREIGN KEY (previous_incident_id)
            REFERENCES sage.incidents (id) ON DELETE SET NULL;
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_incidents_identity_resolved
    ON sage.incidents (identity_key, resolved_at DESC)
    WHERE resolved_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_incidents_resolved_at
    ON sage.incidents (resolved_at)
    WHERE resolved_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_incidents_previous
    ON sage.incidents (previous_incident_id)
    WHERE previous_incident_id IS NOT NULL;
`

func migrateIncidentLifecycle(ctx context.Context, db bootstrapDB) error {
	if _, err := db.Exec(ctx, ddlIncidentLifecycle); err != nil {
		return fmt.Errorf("incident lifecycle columns: %w", err)
	}
	return nil
}
