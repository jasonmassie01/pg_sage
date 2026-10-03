package schema

import (
	"context"
	"fmt"
	"strings"
)

// incidentCheck is one widened CHECK constraint of sage.incidents: the
// values pg_sage writes to column (and NULL when nullable).
type incidentCheck struct {
	name, column string
	values       []string
	nullable     bool
}

// incidentCheckSpecs are the v0.9.1 constraints: log-based RCA sources, info
// severity and the Tier 2 action_risk values.
var incidentCheckSpecs = []incidentCheck{
	{"incidents_severity_check", "severity", []string{"info", "warning", "critical"}, false},
	{"incidents_source_check", "source", []string{"deterministic", "log_deterministic",
		"self_action", "manual_review_required", "llm", "schema_advisor", "schema_lint",
		"n_plus_one"}, false},
	{"incidents_action_risk_check", "action_risk", []string{"safe", "moderate",
		"high_risk", "low", "medium", "high"}, true},
}

// migrateIncidentConstraints widens the CHECK constraints on
// sage.incidents when one still lacks a value pg_sage writes. A
// constraint that admits every value is left alone: re-adding it on every
// startup validated (read) the whole table three times under an ACCESS
// EXCLUSIVE lock (performance gate, v1.8.3). Idempotent.
func migrateIncidentConstraints(
	ctx context.Context, db bootstrapDB,
) error {
	if _, err := db.Exec(ctx, ddlIncidentChecks()); err != nil {
		return err
	}
	return migrateIncidentLifecycle(ctx, db)
}

// ddlIncidentChecks re-creates each existing constraint whose definition
// does not name every value.
func ddlIncidentChecks() string {
	var b strings.Builder
	b.WriteString("DO $$ BEGIN\n")
	for _, c := range incidentCheckSpecs {
		quoted := make([]string, 0, len(c.values))
		names := make([]string, 0, len(c.values))
		for _, v := range c.values {
			quoted = append(quoted, sqlLiteral(v))
			names = append(names, "strpos(pg_get_constraintdef(oid), "+
				sqlLiteral(sqlLiteral(v))+") > 0")
		}
		check := c.column + " IN (" + strings.Join(quoted, ", ") + ")"
		if c.nullable {
			check += " OR " + c.column + " IS NULL"
		}
		fmt.Fprintf(&b, `    IF EXISTS (SELECT 1 FROM pg_constraint
                WHERE conrelid = 'sage.incidents'::regclass AND conname = '%[1]s')
       AND NOT EXISTS (SELECT 1 FROM pg_constraint
                WHERE conrelid = 'sage.incidents'::regclass AND conname = '%[1]s'
                  AND %[2]s) THEN
        ALTER TABLE sage.incidents DROP CONSTRAINT %[1]s;
        ALTER TABLE sage.incidents ADD CONSTRAINT %[1]s CHECK (%[3]s);
    END IF;
`, c.name, strings.Join(names, " AND "), check)
	}
	b.WriteString("END $$;")
	return b.String()
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
