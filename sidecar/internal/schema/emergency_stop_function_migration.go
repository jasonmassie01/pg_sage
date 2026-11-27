package schema

// ddlEmergencyStopFunction (executor-06) is the documented SQL emergency
// stop for when the dashboard and API are down: sage.emergency_stop(stopped,
// actor) upserts the emergency_stop flag and appends the transition to
// sage.config_audit in one transaction, as the API's SetEmergencyStop does.
// The plain UPDATE it replaces matched no row on a database whose stop was
// never toggled through the API. It runs with the caller's privileges
// (SECURITY INVOKER) and a pinned search_path; plpgsql defers the table
// checks to call time, so it installs before sage.config_audit exists on a
// fresh database.
const ddlEmergencyStopFunction = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION sage.emergency_stop(stopped boolean, actor text)
RETURNS boolean
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_new text;
    v_old text;
BEGIN
    IF stopped IS NULL THEN
        RAISE EXCEPTION 'sage.emergency_stop: stopped must be true or false'
            USING ERRCODE = '22004';
    END IF;
    IF actor IS NULL OR btrim(actor) = '' THEN
        RAISE EXCEPTION 'sage.emergency_stop: actor is required (who is stopping or resuming)'
            USING ERRCODE = '22023';
    END IF;
    v_new := CASE WHEN stopped THEN 'true' ELSE 'false' END;
    SELECT c.value INTO v_old FROM sage.config c
     WHERE c.key = 'emergency_stop' AND COALESCE(c.database_id, 0) = 0
     FOR UPDATE;
    INSERT INTO sage.config (key, value, updated_at, updated_by)
    VALUES ('emergency_stop', v_new, now(), actor)
    ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE
       SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by;
    INSERT INTO sage.config_audit (key, old_value, new_value, changed_by_actor)
    VALUES ('emergency_stop', v_old, v_new, actor);
    RETURN stopped;
END
$fn$;
`
