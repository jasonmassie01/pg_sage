package schema

// ddlIncidentAvoidedRevert keeps the incident ledger honest (earned-09).
//
//   - reverted_at: a rollback of the credited action stamps it in the same
//     statement that zeroes the action's toil credit, and the value report
//     skips stamped rows. The row and its credited_minutes stay as evidence.
//   - The kind and severity CHECKs narrow to what a producer writes
//     (xid_wraparound and disk_full_slot near misses). The constraints are
//     added NOT VALID, so the brief lock does not scan the table, and are
//     validated only when no row holds a retired value: an install that has
//     one keeps it as evidence and still refuses new ones. The original
//     wider CHECKs stay; the narrow ones imply them.
//
// Catalog checks first, so a re-run takes no lock on the table;
// lock_timeout bounds the wait of the first run. Every monitored database.
const ddlIncidentAvoidedRevert = `
SET LOCAL lock_timeout = '5s';
DO $incident_avoided_revert$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'sage.incident_avoided'::regclass
                      AND attname = 'reverted_at' AND NOT attisdropped) THEN
        ALTER TABLE sage.incident_avoided
            ADD COLUMN IF NOT EXISTS reverted_at timestamptz;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conrelid = 'sage.incident_avoided'::regclass
                      AND conname = 'incident_avoided_kind_produced') THEN
        ALTER TABLE sage.incident_avoided
            ADD CONSTRAINT incident_avoided_kind_produced
            CHECK (kind IN ('xid_wraparound', 'disk_full_slot')) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conrelid = 'sage.incident_avoided'::regclass
                      AND conname = 'incident_avoided_severity_produced') THEN
        ALTER TABLE sage.incident_avoided
            ADD CONSTRAINT incident_avoided_severity_produced
            CHECK (severity = 'near_miss') NOT VALID;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint
                WHERE conrelid = 'sage.incident_avoided'::regclass
                  AND conname = 'incident_avoided_kind_produced'
                  AND NOT convalidated)
       AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided
                        WHERE kind NOT IN ('xid_wraparound', 'disk_full_slot')) THEN
        ALTER TABLE sage.incident_avoided
            VALIDATE CONSTRAINT incident_avoided_kind_produced;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint
                WHERE conrelid = 'sage.incident_avoided'::regclass
                  AND conname = 'incident_avoided_severity_produced'
                  AND NOT convalidated)
       AND NOT EXISTS (SELECT 1 FROM sage.incident_avoided
                        WHERE severity <> 'near_miss') THEN
        ALTER TABLE sage.incident_avoided
            VALIDATE CONSTRAINT incident_avoided_severity_produced;
    END IF;
END
$incident_avoided_revert$;
`
