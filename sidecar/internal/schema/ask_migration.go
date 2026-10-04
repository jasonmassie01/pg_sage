package schema

// Ask Sage (roadmap phase 3). All idempotent.
//
// sage.ask_conversations and sage.ask_messages hold each user's
// conversations with Ask Sage on the monitored database: the question,
// the answer as served (statements with their citations, what could not
// be verified, the actions taken, the transcript) and its status. A
// conversation belongs to one actor ("user:42", "mcp:token:<id>") and is
// purged ask.retention_days after its last question; its answers cascade.
//
// sage.ask_budget_day is Ask Sage's own persisted daily token budget,
// separate from the investigator's and the tuning agent's: one row per
// (UTC day, actor), and the database's total under actor '*'.
const ddlAsk = `
CREATE TABLE IF NOT EXISTS sage.ask_conversations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor       text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    title       text NOT NULL DEFAULT '' CHECK (length(title) <= 200),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ask_conversations_actor_updated
    ON sage.ask_conversations (actor, updated_at DESC);
CREATE INDEX IF NOT EXISTS ask_conversations_updated
    ON sage.ask_conversations (updated_at);
CREATE TABLE IF NOT EXISTS sage.ask_messages (
    id               bigserial PRIMARY KEY,
    conversation_id  uuid NOT NULL REFERENCES sage.ask_conversations(id) ON DELETE CASCADE,
    question         text NOT NULL CHECK (length(question) BETWEEN 1 AND 8000),
    answer           jsonb NOT NULL,
    status           text NOT NULL CHECK (status IN ('answered', 'not_observed',
                         'budget_exhausted', 'llm_unavailable', 'incomplete')),
    tokens           integer NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ask_messages_conversation
    ON sage.ask_messages (conversation_id, id);
CREATE TABLE IF NOT EXISTS sage.ask_budget_day (
    day         date NOT NULL,
    actor       text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    tokens      bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, actor)
);
`
