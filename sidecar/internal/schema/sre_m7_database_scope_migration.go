package schema

// Earned autonomy per database (2026-10-02 roadmap P0-5, P0-6). Levels
// and proposals were keyed by deployment only, so in a fleet sharing one
// control database one database's evidence promoted another. They are
// now keyed by deployment AND database. Existing rows (decision recorded
// in reviews/2026-10-02-p0-trust-ledger-report.md):
//   - a carried-over level moves to the database its carried_over event
//     names (the database whose configuration seeded it);
//   - any other level keeps database_name '' as a deployment-wide legacy
//     row; each database adopts it once, at its current level, when it
//     binds (internal/earned AdoptLegacy, event 'database_scoped');
//   - a legacy pending proposal is superseded: pg_sage re-evaluates each
//     database on its own evidence.
// Outcomes gain the 'unverified' result: an action without a completed
// verification verdict is recorded as such and earns nothing (P0-6).
// Additive and idempotent: every step checks before it changes.
const ddlSREAutonomyDatabaseScope = `
ALTER TABLE sage.sre_family_autonomy
    ADD COLUMN IF NOT EXISTS database_name text NOT NULL DEFAULT '';
ALTER TABLE sage.sre_autonomy_proposals
    ADD COLUMN IF NOT EXISTS database_name text NOT NULL DEFAULT '';
-- Only a person's review is promotion evidence (coordinator decision
-- 2026-10-02); reviews stored before it were all a person's.
ALTER TABLE sage.sre_packet_reviews
    ADD COLUMN IF NOT EXISTS counts_as_evidence boolean NOT NULL DEFAULT true;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_family_autonomy_db_pkey'
                     AND conrelid = 'sage.sre_family_autonomy'::regclass) THEN
        ALTER TABLE sage.sre_family_autonomy
            DROP CONSTRAINT IF EXISTS sre_family_autonomy_pkey;
        ALTER TABLE sage.sre_family_autonomy
            ADD CONSTRAINT sre_family_autonomy_db_pkey
            PRIMARY KEY (deployment_id, database_name, family, action_class);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_family_autonomy_database_name'
                     AND conrelid = 'sage.sre_family_autonomy'::regclass) THEN
        ALTER TABLE sage.sre_family_autonomy
            ADD CONSTRAINT sre_family_autonomy_database_name
            CHECK (length(database_name) <= 200);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_proposals_database_name'
                     AND conrelid = 'sage.sre_autonomy_proposals'::regclass) THEN
        ALTER TABLE sage.sre_autonomy_proposals
            ADD CONSTRAINT sre_autonomy_proposals_database_name
            CHECK (length(database_name) <= 200);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_outcomes_result_v2'
                     AND conrelid = 'sage.sre_autonomy_outcomes'::regclass) THEN
        ALTER TABLE sage.sre_autonomy_outcomes
            DROP CONSTRAINT IF EXISTS sre_autonomy_outcomes_result_check;
        ALTER TABLE sage.sre_autonomy_outcomes
            ADD CONSTRAINT sre_autonomy_outcomes_result_v2 CHECK (result IN
                ('verified_recovery', 'not_recovered', 'harmful', 'safety_violation',
                 'unverified'));
    END IF;
    -- The event-type check and the one-pending index keep their names
    -- (the earlier migrations re-run before this one and skip existing
    -- names), so they are redefined in place when they lack the scope.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_autonomy_events_type_v2'
                     AND conrelid = 'sage.sre_autonomy_events'::regclass
                     AND pg_get_constraintdef(oid) LIKE '%database_scoped%') THEN
        ALTER TABLE sage.sre_autonomy_events
            DROP CONSTRAINT IF EXISTS sre_autonomy_events_type_v2;
        ALTER TABLE sage.sre_autonomy_events
            ADD CONSTRAINT sre_autonomy_events_type_v2 CHECK (event_type IN (
                'promotion_proposed', 'promotion_approved', 'promotion_rejected',
                'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
                'cap_cleared', 'auto_executed', 'carried_over', 'deadline_override',
                'database_scoped'));
    END IF;
    IF EXISTS (SELECT 1 FROM pg_indexes
               WHERE schemaname = 'sage' AND indexname = 'sre_autonomy_one_pending'
                 AND indexdef NOT LIKE '%database_name%') THEN
        DROP INDEX sage.sre_autonomy_one_pending;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS sre_autonomy_one_pending
    ON sage.sre_autonomy_proposals (deployment_id, database_name, family, action_class)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS sre_autonomy_events_db_pair
    ON sage.sre_autonomy_events (deployment_id, database_name, family, action_class,
                                 created_at DESC);
CREATE INDEX IF NOT EXISTS sre_autonomy_outcomes_db_family
    ON sage.sre_autonomy_outcomes (deployment_id, database_name, family, recorded_at DESC);
CREATE INDEX IF NOT EXISTS sre_packet_reviews_db_family
    ON sage.sre_packet_reviews (deployment_id, database_name, family, reviewed_at);

-- Each deployment-wide carried-over level looks up its own pair's first
-- carry-over event (sre_autonomy_events_pair); with no such level the
-- event history is not read at all (it was, on every startup: v1.8.3).
UPDATE sage.sre_family_autonomy a
   SET database_name = e.database_name
  FROM sage.sre_family_autonomy l
  CROSS JOIN LATERAL (
        SELECT ev.database_name
          FROM sage.sre_autonomy_events ev
         WHERE ev.deployment_id = l.deployment_id AND ev.family = l.family
           AND ev.action_class = l.action_class AND ev.event_type = 'carried_over'
           AND COALESCE(ev.database_name, '') <> ''
         ORDER BY ev.created_at, ev.id
         LIMIT 1) e
 WHERE l.database_name = '' AND l.provenance = 'carried_over'
   AND a.deployment_id = l.deployment_id AND a.database_name = ''
   AND a.family = l.family AND a.action_class = l.action_class
   AND NOT EXISTS (SELECT 1 FROM sage.sre_family_autonomy b
                    WHERE b.deployment_id = a.deployment_id
                      AND b.database_name = e.database_name
                      AND b.family = a.family AND b.action_class = a.action_class);

UPDATE sage.sre_autonomy_proposals
   SET status = 'superseded', decided_by = 'pg_sage', decided_at = clock_timestamp(),
       decision_note = 'superseded: the ledger is per database now; pg_sage ' ||
                       're-evaluates each database on its own evidence'
 WHERE database_name = '' AND status = 'pending';
`
