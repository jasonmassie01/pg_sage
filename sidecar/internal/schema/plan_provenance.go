package schema

// ddlExplainCacheProvenance keeps provider observations distinct from local
// estimates. Nullable flags mean unknown, never an analyzed execution. The
// occurrence key fences sink replay after a control-inbox commit/ACK crash.
const ddlExplainCacheProvenance = `
ALTER TABLE sage.explain_cache
    ADD COLUMN IF NOT EXISTS provider_scope_hash text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider_compute_id text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS provider_event_id text,
    ADD COLUMN IF NOT EXISTS comparison_key text,
    ADD COLUMN IF NOT EXISTS received_at timestamptz,
    ADD COLUMN IF NOT EXISTS analyze_enabled boolean,
    ADD COLUMN IF NOT EXISTS buffers_enabled boolean,
    ADD COLUMN IF NOT EXISTS timing_enabled boolean,
    ADD COLUMN IF NOT EXISTS provider_plan_fingerprint text,
    ADD COLUMN IF NOT EXISTS duration_source text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_explain_provider_event
    ON sage.explain_cache(provider_scope_hash, provider_event_id)
    WHERE provider_event_id IS NOT NULL;
`
