package schema

// ddlAskRuns (mcp-07b) creates sage.ask_runs: Ask Sage's asynchronous
// runs. A run is one question answered in the background: its state, the
// tools it called so far (progress: at most 64 entries of tool, status,
// action id and time; never tool output), the stored answer once it ends
// and the error of a failed run. instance is the sidecar that runs it; at
// start a sidecar marks only its own stale runs interrupted, so HA peers
// leave each other's runs alone. A run goes with its conversation
// (ON DELETE CASCADE), so retention needs no rule of its own.
//
// CREATE ... IF NOT EXISTS on a new table and its indexes; lock_timeout
// bounds the wait for the foreign keys' share locks on the first run.
const ddlAskRuns = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.ask_runs (
    id               uuid PRIMARY KEY,
    conversation_id  uuid NOT NULL REFERENCES sage.ask_conversations(id) ON DELETE CASCADE,
    actor            text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    question         text NOT NULL CHECK (length(question) BETWEEN 1 AND 8000),
    read_only        boolean NOT NULL,
    state            text NOT NULL CHECK (state IN
                         ('queued', 'running', 'done', 'cancelled', 'interrupted', 'failed')),
    progress         jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(progress) = 'array'
                         AND jsonb_array_length(progress) <= 64),
    message_id       bigint REFERENCES sage.ask_messages(id) ON DELETE SET NULL,
    error            text CHECK (error IS NULL OR length(error) <= 500),
    instance         text NOT NULL CHECK (length(instance) BETWEEN 1 AND 200),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);
CREATE INDEX IF NOT EXISTS ask_runs_actor_created
    ON sage.ask_runs (actor, created_at DESC);
CREATE INDEX IF NOT EXISTS ask_runs_active
    ON sage.ask_runs (conversation_id, instance) WHERE state IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS ask_runs_message
    ON sage.ask_runs (message_id) WHERE message_id IS NOT NULL;
`
