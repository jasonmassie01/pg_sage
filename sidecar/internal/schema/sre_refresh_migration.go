package schema

// playbooks-06: an operator's refresh of a finished investigation's
// evidence appends an evidence_refreshed event, which attests to the
// refresh row. The refresh step and evidence row fit the existing
// sre_steps and sre_evidence columns (source_kind has no check), so the
// only change is admitting the event type in the single sre_events
// event-type check, rebuilt as ddlSREAttemptEventTypes does. A re-run
// that finds the type admitted returns before taking any lock;
// lock_timeout bounds the first run's wait.
const ddlSREEvidenceRefreshedEvent = `
SET LOCAL lock_timeout = '5s';
DO $sre_refresh_events$
DECLARE
    wanted  text[] := ARRAY['evidence_refreshed'];
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
$sre_refresh_events$;
`
