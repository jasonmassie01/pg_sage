package schema

import (
	"context"
	"fmt"
)

// retentionFKSQL converts nullable references to purgeable rows into
// ON DELETE SET NULL (G1-B12 / G7-B11). With NO ACTION, one surviving
// child row (an alert for a resolved finding, a queue entry, a decision
// pointing at an old action) made retention's batched DELETE fail and
// the whole table was never purged again.
//
// SET NULL (not CASCADE) because every child here is an audit or
// current-state row that must outlive the parent: the alert/decision
// record stays, only the link to the expired parent is cleared. NOT NULL
// references (verification/change_lease/incident_avoided -> decision,
// incident_avoided -> action_log/verification) are left alone: retention
// skips parents that still have such children.
//
// Idempotent: only constraints whose delete action differs are rebuilt,
// under their existing names; tables that do not exist are skipped.
const retentionFKSQL = `
DO $$
DECLARE
    r   record;
    con record;
BEGIN
    FOR r IN SELECT * FROM (VALUES
        ('alert_log',       'finding_id',                  'findings'),
        ('findings',        'action_log_id',               'action_log'),
        ('action_queue',    'action_log_id',               'action_log'),
        ('decision',        'action_log_id',               'action_log'),
        ('decision',        'queue_id',                    'action_queue'),
        ('verification',    'action_log_id',               'action_log'),
        ('schema_baseline', 'last_authorized_action_id',   'action_log'),
        ('schema_baseline', 'last_authorized_decision_id', 'decision'),
        ('action_log',      'decision_id',                 'decision'),
        ('action_log',      'verification_id',             'verification')
    ) AS v(tbl, col, ref)
    LOOP
        IF to_regclass('sage.' || r.tbl) IS NULL
           OR to_regclass('sage.' || r.ref) IS NULL THEN
            CONTINUE;
        END IF;
        FOR con IN
            SELECT c.conname
              FROM pg_constraint c
              JOIN pg_attribute a
                ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
             WHERE c.contype = 'f'
               AND c.conrelid = to_regclass('sage.' || r.tbl)
               AND c.confrelid = to_regclass('sage.' || r.ref)
               AND a.attname = r.col
               AND cardinality(c.conkey) = 1
               AND c.confdeltype <> 'n'
        LOOP
            EXECUTE format('ALTER TABLE sage.%I DROP CONSTRAINT %I',
                           r.tbl, con.conname);
            EXECUTE format('ALTER TABLE sage.%I ADD CONSTRAINT %I FOREIGN KEY (%I) '
                           'REFERENCES sage.%I(id) ON DELETE SET NULL',
                           r.tbl, con.conname, r.col, r.ref);
        END LOOP;
    END LOOP;
END $$;`

// migrateRetentionForeignKeys applies retentionFKSQL.
func migrateRetentionForeignKeys(ctx context.Context, db bootstrapDB) error {
	if _, err := db.Exec(ctx, retentionFKSQL); err != nil {
		return fmt.Errorf("retention foreign keys: %w", err)
	}
	return nil
}
