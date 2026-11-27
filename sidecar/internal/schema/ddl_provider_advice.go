package schema

// ddlProviderAdvice stores the managed provider's own advisor
// recommendations reconciled with pg_sage's findings (market-09): one row
// per database, provider and provider ID, with the verdict (agree,
// pgsage_only, provider_only, conflict, unparsed, unknown) and why, and
// each source's last fetch. Evidence only: nothing in the executor reads
// it. finding_id has no foreign key because findings are pruned by
// retention. CREATE IF NOT EXISTS takes no lock on a re-run; lock_timeout
// bounds the first.
const ddlProviderAdvice = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.provider_recommendations (
    id          bigserial PRIMARY KEY,
    database    text NOT NULL,
    provider    text NOT NULL,
    provider_id text NOT NULL,
    kind        text NOT NULL
        CONSTRAINT provider_recommendations_kind_check
        CHECK (kind IN ('create_index', 'drop_index', 'unknown')),
    payload     jsonb NOT NULL DEFAULT '{}'
        CONSTRAINT provider_recommendations_payload_object
        CHECK (jsonb_typeof(payload) = 'object'),
    verdict     text NOT NULL
        CONSTRAINT provider_recommendations_verdict_check
        CHECK (verdict IN ('agree', 'pgsage_only', 'provider_only', 'conflict',
                           'unparsed', 'unknown')),
    reason      text NOT NULL DEFAULT '',
    finding_id  bigint,
    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    CONSTRAINT provider_recommendations_identity UNIQUE (database, provider, provider_id)
);
CREATE INDEX IF NOT EXISTS idx_provider_recommendations_finding
    ON sage.provider_recommendations (finding_id)
    WHERE finding_id IS NOT NULL AND resolved_at IS NULL;
CREATE TABLE IF NOT EXISTS sage.provider_advice_sources (
    database     text NOT NULL,
    provider     text NOT NULL,
    last_attempt timestamptz NOT NULL DEFAULT now(),
    last_success timestamptz,
    last_error   text NOT NULL DEFAULT '',
    PRIMARY KEY (database, provider)
);
`
