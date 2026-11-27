package schema

// sre-01: the runs of one idempotency key are numbered attempts. When the
// newest run of a still-open trigger expired (in the queue or on stale
// evidence), the trigger's next poll starts attempt n+1, which names the
// run it supersedes. The key is therefore unique per attempt instead of
// over all time (the old index is replaced). Rows stored before are
// attempt 1. At most 5 attempts per key, like the other R1 ceilings, and
// a predecessor exactly when attempt > 1. supersedes is not a foreign
// key: retention deletes runs one by one, oldest first. The event chain
// admits attempts_exhausted. The catalog checks make a re-run take no
// lock on sage.sre_investigations; lock_timeout bounds the first run's
// wait. After ddlSRECoordination and the event-type checks of M3/M5.
const ddlSREAttempts = `
SET LOCAL lock_timeout = '5s';
DO $sre_attempts$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'sage.sre_investigations'::regclass
                      AND attname = 'attempt' AND NOT attisdropped) THEN
        ALTER TABLE sage.sre_investigations
            ADD COLUMN attempt integer NOT NULL DEFAULT 1,
            ADD COLUMN supersedes uuid,
            ADD CONSTRAINT sre_investigations_attempt CHECK (attempt BETWEEN 1 AND 5
                AND (attempt = 1) = (supersedes IS NULL));
    END IF;
    IF to_regclass('sage.sre_investigation_attempt') IS NULL THEN
        CREATE UNIQUE INDEX sre_investigation_attempt ON sage.sre_investigations
            (deployment_id, database_id, idempotency_key, attempt)
            WHERE idempotency_key IS NOT NULL;
    END IF;
    IF to_regclass('sage.sre_investigation_idempotency') IS NOT NULL THEN
        DROP INDEX sage.sre_investigation_idempotency;
    END IF;
    IF to_regclass('sage.sre_investigation_supersedes') IS NULL THEN
        CREATE INDEX sre_investigation_supersedes ON sage.sre_investigations
            (deployment_id, database_id, supersedes) WHERE supersedes IS NOT NULL;
    END IF;
END
$sre_attempts$;
` + ddlSREAttemptEventTypes

// ddlSREAttemptEventTypes adds attempts_exhausted to the single
// sre_events event-type check, rebuilt as ddlSREActionEventTypes does.
const ddlSREAttemptEventTypes = `
DO $sre_attempt_events$
DECLARE
    wanted  text[] := ARRAY['attempts_exhausted'];
    allowed text[] := '{}';
    checks  int := 0;
    named   boolean := false;
    c       record;
BEGIN
    FOR c IN SELECT conname, pg_get_constraintdef(oid) AS def FROM pg_constraint
             WHERE conrelid = 'sage.sre_events'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%event_type%' LOOP
        checks := checks + 1;
        named := named OR c.conname = 'sre_events_event_type_m3';
        allowed := allowed || ARRAY(SELECT (regexp_matches(c.def,
            '''([a-z0-9_]+)''', 'g'))[1]);
    END LOOP;
    IF checks = 1 AND named AND wanted <@ allowed THEN
        RETURN;
    END IF;
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'sage.sre_events'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%event_type%' LOOP
        EXECUTE format('ALTER TABLE sage.sre_events DROP CONSTRAINT %I', c.conname);
    END LOOP;
    EXECUTE (SELECT format('ALTER TABLE sage.sre_events ADD CONSTRAINT '
        'sre_events_event_type_m3 CHECK (event_type IN (%s)) NOT VALID',
        string_agg(quote_literal(v), ', ' ORDER BY v))
        FROM (SELECT DISTINCT unnest(allowed || wanted) AS v) u);
    ALTER TABLE sage.sre_events VALIDATE CONSTRAINT sre_events_event_type_m3;
END
$sre_attempt_events$;
`
