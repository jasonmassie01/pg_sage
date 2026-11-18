package schema

// ddlReviewModelRuns (review-03a): one row per model run of a review's
// model pass. raw_output is the model's replies, capped at 256 KiB with
// raw_truncated set; output_sha256 hashes the whole output, so a plan's
// audit record can cite it and replay the pack against a later model.
// cost_usd is NULL when the model is unpriced. The table is append-only:
// a run is written once, with its final status, and a trigger refuses
// every UPDATE. Rows go with their review run (ON DELETE CASCADE).
//
// sage.llm_usage's role check gains 'review', the dedicated review
// client's counter (llm.review_llm). The check is swapped only while it
// lacks the role, so a re-run takes no lock.
const ddlReviewModelRuns = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_model_run (
    run_id         bigint NOT NULL REFERENCES sage.review_run (id) ON DELETE CASCADE,
    attempt        int NOT NULL CHECK (attempt BETWEEN 1 AND 5),
    role           text NOT NULL CHECK (role IN ('rank', 'refute')),
    model          text NOT NULL DEFAULT '' CHECK (length(model) <= 200),
    prompt_version text NOT NULL CHECK (length(prompt_version) BETWEEN 1 AND 100),
    pack_hash      text NOT NULL CHECK (pack_hash ~ '^[0-9a-f]{64}$'),
    raw_output     text NOT NULL DEFAULT '' CHECK (octet_length(raw_output) <= 262144),
    raw_truncated  boolean NOT NULL DEFAULT false,
    output_sha256  text NOT NULL CHECK (output_sha256 ~ '^[0-9a-f]{64}$'),
    kept_claims    int NOT NULL DEFAULT 0 CHECK (kept_claims >= 0),
    dropped_claims jsonb NOT NULL DEFAULT '[]'::jsonb,
    tokens_in      bigint NOT NULL DEFAULT 0 CHECK (tokens_in >= 0),
    tokens_out     bigint NOT NULL DEFAULT 0 CHECK (tokens_out >= 0),
    cost_usd       numeric(20, 8) CHECK (cost_usd >= 0),
    status         text NOT NULL CHECK (status IN ('done', 'failed', 'skipped')),
    error_class    text NOT NULL DEFAULT '' CHECK (length(error_class) <= 40),
    reason         text NOT NULL DEFAULT '' CHECK (length(reason) <= 1000),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, attempt)
);
CREATE OR REPLACE FUNCTION sage.review_model_run_append_only() RETURNS trigger
LANGUAGE plpgsql AS $review_model_run$
BEGIN
    RAISE EXCEPTION 'sage.review_model_run is append-only: run % attempt % is final',
        OLD.run_id, OLD.attempt;
END
$review_model_run$;
DO $review_model_run_ddl$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger
                   WHERE tgrelid = 'sage.review_model_run'::regclass
                     AND tgname = 'review_model_run_append_only') THEN
        CREATE TRIGGER review_model_run_append_only BEFORE UPDATE
            ON sage.review_model_run
            FOR EACH ROW EXECUTE FUNCTION sage.review_model_run_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'sage.llm_usage'::regclass
                     AND conname = 'llm_usage_role_check'
                     AND pg_get_constraintdef(oid) LIKE '%''review''%') THEN
        ALTER TABLE sage.llm_usage DROP CONSTRAINT IF EXISTS llm_usage_role_check;
        ALTER TABLE sage.llm_usage ADD CONSTRAINT llm_usage_role_check
            CHECK (role IN ('general', 'optimizer', 'shared', 'fleet', 'review'));
    END IF;
END
$review_model_run_ddl$;
`
