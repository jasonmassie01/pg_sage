package schema

// LLM usage ledger (fleet-10). Idempotent.
//
// sage.llm_usage keeps today's LLM token spend in the control database so
// a restart mid-day restores it instead of granting a fresh daily budget.
// database_key is id:<sage.databases id> for a meta-DB database,
// name:<name> for a YAML or standalone one, and process for the
// process-wide clients; role names the counter (general, optimizer, the
// shared global ceiling, or the fleet per-database share). Flushes only
// raise tokens, so a stale writer cannot lower it.
const ddlLLMUsage = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.llm_usage (
    day          date NOT NULL,
    database_key text NOT NULL CHECK (database_key <> ''),
    role         text NOT NULL
                 CHECK (role IN ('general', 'optimizer', 'shared', 'fleet')),
    tokens       bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, database_key, role)
);
`
