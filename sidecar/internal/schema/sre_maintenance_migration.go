package schema

// Operator-requested maintenance has a separate typed ledger. The cancel-only
// M5 proposal schema and its automatic proposal path remain unchanged.
const ddlSREMaintenance = `
CREATE TABLE IF NOT EXISTS sage.sre_maintenance_proposals (
 id uuid PRIMARY KEY,
 deployment_id uuid NOT NULL,
 database_id uuid NOT NULL,
 investigation_id uuid NOT NULL,
 state text NOT NULL CHECK(state IN ('proposed','requested','executing','executed','uncertain','failed','refused','rolling_back','rolled_back','rollback_failed')),
 proposal jsonb NOT NULL CHECK(jsonb_typeof(proposal)='object' AND octet_length(proposal::text)<=65536),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(deployment_id,database_id,investigation_id),
 FOREIGN KEY(deployment_id,database_id,investigation_id) REFERENCES sage.sre_investigations(deployment_id,database_id,id) ON DELETE CASCADE
);
-- Retention correlates an executor's local finding and an immutable queue
-- identity. Index the same text expressions used by its keep predicates so
-- even large/TOASTed proposal bodies are not scanned per purge candidate.
CREATE INDEX IF NOT EXISTS idx_sre_maintenance_proposals_finding
 ON sage.sre_maintenance_proposals ((proposal->>'finding_id'));
CREATE INDEX IF NOT EXISTS idx_sre_maintenance_proposals_identity
 ON sage.sre_maintenance_proposals (('sre_maintenance:' || id::text));
` + ddlSREMaintenanceStates

// CREATE TABLE IF NOT EXISTS cannot widen an earlier install's check. The
// nine-state prototype omitted uncertain, even though the durable execution
// path can record it. Replace only the table's owned named state check with
// the canonical supported set; do not silently admit arbitrary future states.
// Validate existing rows in the same transaction. A current validated check
// takes the fast path without another ALTER/validation on each bootstrap.
const ddlSREMaintenanceStates = `
DO $$
DECLARE
    wanted text[] := ARRAY['proposed','requested','executing','executed','uncertain',
        'failed','refused','rolling_back','rolled_back','rollback_failed'];
    definition text;
    validated boolean;
    canonical text;
BEGIN
    SELECT pg_get_constraintdef(oid), convalidated INTO definition, validated
    FROM pg_constraint
    WHERE conrelid = 'sage.sre_maintenance_proposals'::regclass
      AND contype = 'c' AND conname = 'sre_maintenance_proposals_state_check';
    -- Compare the entire canonical expression, including its operator. Merely
    -- matching literals would also accept NOT IN or an added OR true clause.
    SELECT 'CHECK ((state = ANY (ARRAY[' ||
        string_agg(quote_literal(value) || '::text', ', ' ORDER BY position) || '])))'
        INTO canonical
        FROM unnest(wanted) WITH ORDINALITY AS states(value, position);
    IF validated AND definition = canonical THEN
        RETURN;
    END IF;
    ALTER TABLE sage.sre_maintenance_proposals
        DROP CONSTRAINT IF EXISTS sre_maintenance_proposals_state_check;
    ALTER TABLE sage.sre_maintenance_proposals
        ADD CONSTRAINT sre_maintenance_proposals_state_check
        CHECK (state IN ('proposed','requested','executing','executed','uncertain',
            'failed','refused','rolling_back','rolled_back','rollback_failed')) NOT VALID;
    ALTER TABLE sage.sre_maintenance_proposals
        VALIDATE CONSTRAINT sre_maintenance_proposals_state_check;
END $$;
`
