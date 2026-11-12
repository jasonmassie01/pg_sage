package schema

// LLM usage attribution (llm-04). Idempotent.
//
// Extends fleet-10's sage.llm_usage instead of adding a second store: an
// attribution row names the purpose and model of the calls and adds their
// prompt, completion and reasoning tokens, calls, failures and the
// operator-priced cost (NULL without a price). fleet-10's budget counters
// keep purpose '_total' and an empty model, so the existing rows backfill to
// them and the budget flush is unchanged. The primary key gains purpose
// and model; it is swapped only while it still has fleet-10's shape.
const ddlLLMUsageAttribution = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.llm_usage
    ADD COLUMN IF NOT EXISTS purpose text NOT NULL DEFAULT '_total'
        CHECK (purpose ~ '^(_total|[a-z][a-z0-9_]{0,62})$'),
    ADD COLUMN IF NOT EXISTS model text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS prompt_tokens bigint NOT NULL DEFAULT 0
        CHECK (prompt_tokens >= 0),
    ADD COLUMN IF NOT EXISTS completion_tokens bigint NOT NULL DEFAULT 0
        CHECK (completion_tokens >= 0),
    ADD COLUMN IF NOT EXISTS reasoning_tokens bigint NOT NULL DEFAULT 0
        CHECK (reasoning_tokens >= 0),
    ADD COLUMN IF NOT EXISTS calls bigint NOT NULL DEFAULT 0 CHECK (calls >= 0),
    ADD COLUMN IF NOT EXISTS failures bigint NOT NULL DEFAULT 0 CHECK (failures >= 0),
    ADD COLUMN IF NOT EXISTS est_cost_usd numeric(20, 8) CHECK (est_cost_usd >= 0),
    ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'provider'
        CHECK (source IN ('provider', 'estimate'));
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.llm_usage'::regclass AND contype = 'p'
           AND pg_get_constraintdef(oid) =
               'PRIMARY KEY (day, database_key, role, purpose, model)') THEN
        ALTER TABLE sage.llm_usage DROP CONSTRAINT IF EXISTS llm_usage_pkey;
        ALTER TABLE sage.llm_usage ADD CONSTRAINT llm_usage_pkey
            PRIMARY KEY (day, database_key, role, purpose, model);
    END IF;
END $$;
`
