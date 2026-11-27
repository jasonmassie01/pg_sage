package schema

// ddlIncidentsLogOccurrences (rca-07) stores a log incident's occurrence
// evidence: how often the line occurred, the PIDs (for a deadlock, its
// participants) and the first and last sighting. Nullable: metric
// incidents and rows from older versions have none. The catalog check
// keeps the AccessExclusive lock of ALTER TABLE to the first bootstrap.
const ddlIncidentsLogOccurrences = `
SET LOCAL lock_timeout = '5s';
DO $incidents_log_occurrences$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'sage.incidents'::regclass
                      AND attname = 'log_occurrences' AND NOT attisdropped) THEN
        ALTER TABLE sage.incidents ADD COLUMN IF NOT EXISTS log_occurrences jsonb;
    END IF;
END
$incidents_log_occurrences$;
`
