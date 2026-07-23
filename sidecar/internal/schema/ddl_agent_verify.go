package schema

const ddlAgentNativeVerification = `
CREATE TABLE IF NOT EXISTS sage.verification (
    id                  bigserial PRIMARY KEY,
    database_id         bigint,
    decision_id         bigint NOT NULL REFERENCES sage.decision(id),
    action_log_id       bigint REFERENCES sage.action_log(id),
    criterion           jsonb NOT NULL,
    baseline            jsonb NOT NULL,
    minimum_samples     integer NOT NULL,
    next_evaluation_at  timestamptz NOT NULL,
    hard_deadline_at    timestamptz NOT NULL,
    verdict             text NOT NULL DEFAULT 'pending',
    reason              text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    completed_at        timestamptz,
    CHECK (minimum_samples > 0),
    CHECK (verdict IN (
        'pending', 'extended', 'success', 'revert', 'failed', 'unverifiable'
    ))
);
CREATE INDEX IF NOT EXISTS idx_verification_due
    ON sage.verification (next_evaluation_at)
    WHERE verdict IN ('pending', 'extended');
CREATE INDEX IF NOT EXISTS idx_verification_action
    ON sage.verification (action_log_id)
    WHERE action_log_id IS NOT NULL;

ALTER TABLE sage.action_log
    ADD COLUMN IF NOT EXISTS verification_id bigint REFERENCES sage.verification(id);
CREATE INDEX IF NOT EXISTS idx_action_log_verification
    ON sage.action_log (verification_id)
    WHERE verification_id IS NOT NULL;
`
