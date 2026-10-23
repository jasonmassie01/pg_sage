package schema

// Change Review. All idempotent.
//
// sage.change_review holds one review of a migration or data-access change:
// the request's identity (PR, commit, tool, hashes; never the raw diff,
// only its sha256), the analysis (statements, findings, evidence, what was
// dropped or not determined, the expectations the receipt checks), the
// rendered comment, the DBA's verdict, the landing bracket, the receipt,
// the shadow auto-clear score and the value credit. Every transition is a
// conditional UPDATE in internal/changereview. The landing and receipt
// columns are NULL until the review completes.
//
// sage.change_review_budget_day is Change Review's persisted daily token
// budget: one row per (UTC day, actor), the database total under '*'.
//
// sage.toil_model gains the 'change_review' unit of credit: 15 minutes
// for a review a DBA concurred with whose receipt came back clean.
const ddlChangeReview = `
CREATE TABLE IF NOT EXISTS sage.change_review (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    database_name         text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    source                text NOT NULL
        CONSTRAINT change_review_source_check CHECK (source IN ('mcp', 'api', 'cli')),
    actor                 text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    repo                  text CHECK (repo IS NULL OR length(repo) <= 300),
    pr_url                text CHECK (pr_url IS NULL OR length(pr_url) <= 2048),
    pr_number             integer CHECK (pr_number IS NULL OR pr_number > 0),
    head_sha              text CHECK (head_sha IS NULL OR head_sha ~ '^[0-9a-f]{7,64}$'),
    base_sha              text CHECK (base_sha IS NULL OR base_sha ~ '^[0-9a-f]{7,64}$'),
    pr_title              text CHECK (pr_title IS NULL OR length(pr_title) <= 300),
    pr_opened_at          timestamptz,
    migration_tool        text CHECK (migration_tool IS NULL OR length(migration_tool) <= 40),
    transactional         boolean,
    revision_of           uuid,
    input_hash            text NOT NULL CHECK (length(input_hash) = 64),
    content_hash          text CHECK (content_hash IS NULL OR length(content_hash) = 64),
    diff_sha256           text CHECK (diff_sha256 IS NULL OR length(diff_sha256) = 64),
    state                 text NOT NULL DEFAULT 'running'
        CONSTRAINT change_review_state_check
        CHECK (state IN ('running', 'complete', 'failed')),
    failure_reason        text CHECK (failure_reason IS NULL OR length(failure_reason) <= 2000),
    stage                 text CHECK (stage IS NULL OR length(stage) <= 40),
    verdict               text CONSTRAINT change_review_verdict_check
        CHECK (verdict IS NULL OR verdict IN ('safe', 'review', 'risky')),
    deterministic_verdict text CONSTRAINT change_review_deterministic_verdict_check
        CHECK (deterministic_verdict IS NULL
               OR deterministic_verdict IN ('safe', 'review', 'risky')),
    parse_mode            text CONSTRAINT change_review_parse_mode_check
        CHECK (parse_mode IS NULL OR parse_mode IN ('ast', 'text')),
    model_status          text CONSTRAINT change_review_model_status_check
        CHECK (model_status IS NULL OR model_status IN ('ok', 'no_model', 'budget',
               'rate_limited', 'timeout', 'error', 'skipped')),
    statements            jsonb NOT NULL DEFAULT '[]'::jsonb,
    findings              jsonb NOT NULL DEFAULT '[]'::jsonb,
    evidence              jsonb NOT NULL DEFAULT '[]'::jsonb,
    dropped               jsonb NOT NULL DEFAULT '[]'::jsonb,
    not_determined        jsonb NOT NULL DEFAULT '[]'::jsonb,
    summary               jsonb NOT NULL DEFAULT '{}'::jsonb,
    expectations          jsonb NOT NULL DEFAULT '{}'::jsonb,
    transcript            jsonb,
    markdown              text,
    tokens                integer NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    receipt_hash          text CHECK (receipt_hash IS NULL OR length(receipt_hash) = 64),
    dba_verdict           text CONSTRAINT change_review_dba_verdict_check
        CHECK (dba_verdict IS NULL
               OR dba_verdict IN ('concur', 'concur_with_edits', 'override')),
    dba_direction         text CONSTRAINT change_review_dba_direction_check
        CHECK (dba_direction IS NULL OR dba_direction IN ('too_strict', 'too_lenient')),
    dba_reason            text CHECK (dba_reason IS NULL OR length(dba_reason) <= 2000),
    dba_actor             text CHECK (dba_actor IS NULL OR length(dba_actor) BETWEEN 1 AND 200),
    dba_decided_at        timestamptz,
    verdict_carried_from  uuid,
    landing_state         text CONSTRAINT change_review_landing_state_check
        CHECK (landing_state IS NULL
               OR landing_state IN ('awaiting', 'landed', 'expired', 'not_applicable')),
    landing_source        text CONSTRAINT change_review_landing_source_check
        CHECK (landing_source IS NULL
               OR landing_source IN ('catalog', 'change_event', 'reported')),
    landed_lower          timestamptz,
    landed_at             timestamptz,
    receipt_verdict       text CONSTRAINT change_review_receipt_verdict_check
        CHECK (receipt_verdict IS NULL OR receipt_verdict IN ('pending', 'clean',
               'clean_low_traffic', 'regressed', 'unverifiable')),
    receipt               jsonb,
    receipt_decided_at    timestamptz,
    shadow_auto_clear     boolean,
    shadow_score          text NOT NULL DEFAULT 'unscored'
        CONSTRAINT change_review_shadow_score_check
        CHECK (shadow_score IN ('correct', 'incorrect', 'unscored')),
    credit_state          text NOT NULL DEFAULT 'none'
        CONSTRAINT change_review_credit_state_check
        CHECK (credit_state IN ('none', 'pending', 'final', 'void')),
    credited_minutes      integer NOT NULL DEFAULT 0 CHECK (credited_minutes >= 0),
    toil_model_version    integer,
    credit_final_at       timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    completed_at          timestamptz,
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT change_review_override_reason_check
        CHECK (dba_verdict IS DISTINCT FROM 'override'
               OR (length(btrim(coalesce(dba_reason, ''))) > 0 AND dba_direction IS NOT NULL)),
    CONSTRAINT change_review_complete_check
        CHECK (state <> 'complete'
               OR (verdict IS NOT NULL AND deterministic_verdict IS NOT NULL
                   AND parse_mode IS NOT NULL AND model_status IS NOT NULL
                   AND landing_state IS NOT NULL AND receipt_verdict IS NOT NULL
                   AND completed_at IS NOT NULL)),
    CONSTRAINT change_review_landed_check
        CHECK (landing_state IS DISTINCT FROM 'landed'
               OR (landing_source IS NOT NULL AND landed_at IS NOT NULL
                   AND landed_lower IS NOT NULL AND landed_lower <= landed_at)),
    CONSTRAINT change_review_credit_final_check
        CHECK (credit_state <> 'final' OR credit_final_at IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS change_review_landing_awaiting
    ON sage.change_review (completed_at) WHERE landing_state = 'awaiting';
CREATE INDEX IF NOT EXISTS change_review_receipt_due
    ON sage.change_review (landed_at)
    WHERE landing_state = 'landed' AND receipt_verdict = 'pending';
CREATE INDEX IF NOT EXISTS idx_change_review_credit
    ON sage.change_review (credit_final_at) INCLUDE (credited_minutes)
    WHERE credit_state = 'final';
CREATE INDEX IF NOT EXISTS change_review_pr_created
    ON sage.change_review (pr_url, created_at DESC);
CREATE INDEX IF NOT EXISTS change_review_created
    ON sage.change_review (created_at DESC);
CREATE INDEX IF NOT EXISTS change_review_dba_queue
    ON sage.change_review (completed_at)
    WHERE state = 'complete' AND dba_verdict IS NULL;
CREATE INDEX IF NOT EXISTS change_review_input_hash
    ON sage.change_review (input_hash, created_at DESC);
CREATE INDEX IF NOT EXISTS change_review_running
    ON sage.change_review (updated_at) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS change_review_updated
    ON sage.change_review (updated_at);
CREATE TABLE IF NOT EXISTS sage.change_review_budget_day (
    day         date NOT NULL,
    actor       text NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    tokens      bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (day, actor)
);
INSERT INTO sage.toil_model (action_type, model_version, base_minutes, notes, provenance)
VALUES ('change_review', 1, 15,
        'Gather live context for a database change and review it before merge',
        'change_review_v1')
ON CONFLICT DO NOTHING;
`
