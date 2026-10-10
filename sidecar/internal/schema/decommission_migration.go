package schema

// ddlDecommission supports the AgentDB decommission (AGENTDB-SPEC §12, G0-07).
//
//   - sage.agentdb_decommission records the operator's acknowledgement of
//     each inventoried resource: by whom, through the API or the config, and
//     only once the inventory was exported. It is the one out-of-gate write
//     the spec allows for removed code (§6.2.5).
//   - Persisted agentdb.* overrides configure removed code. They are deleted,
//     each with a sage.config_audit row that keeps the old value. The delete
//     waits until sage.config_audit (with changed_by_actor) exists, so it
//     never deletes without its audit row; it is idempotent.
//
// The 27 legacy AgentDB tables are not dropped here: the inventory reads
// them, and §12 step 5 drops them in G1.
const ddlDecommission = `
CREATE TABLE IF NOT EXISTS sage.agentdb_decommission (
    resource_id     text PRIMARY KEY,
    exported        boolean NOT NULL CHECK (exported),
    acknowledged_by text NOT NULL CHECK (acknowledged_by <> ''),
    source          text NOT NULL CHECK (source IN ('api', 'yaml')),
    acknowledged_at timestamptz NOT NULL DEFAULT now()
);
/* pg_sage agentdb_decommission v1 */
DO $decommission$
BEGIN
    IF to_regclass('sage.config_audit') IS NULL OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_attribute
        WHERE attrelid = to_regclass('sage.config_audit')
          AND attname = 'changed_by_actor' AND NOT attisdropped) OR NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_attribute
        WHERE attrelid = to_regclass('sage.config')
          AND attname = 'database_id' AND NOT attisdropped) THEN
        RETURN;
    END IF;
    WITH removed AS (
        DELETE FROM sage.config WHERE key LIKE 'agentdb.%'
        RETURNING key, value, database_id
    )
    INSERT INTO sage.config_audit (key, old_value, new_value, database_id,
                                   changed_by_actor)
    SELECT key, value, '', database_id,
           'pg_sage: AgentDB provisioning removed (AGENTDB-SPEC §12)'
    FROM removed;
END
$decommission$;
`
