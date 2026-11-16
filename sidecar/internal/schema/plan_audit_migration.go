package schema

// ddlPlanEvents (plan-01b) adds a plan's audit record, beside the plan
// tables in the monitored database's sage schema (internal/changeplan):
//
//   - sage.plan_event: one row per plan write and engine transition,
//     written in the transaction of the change it records, with the plan
//     hash after it. The kind vocabulary is set up front.
//   - sage.plan_model_record: one row per model pass that contributed to a
//     plan (pack hash, model, prompt version, the raw output byte for byte
//     up to 1 MiB and its SHA-256), the input a later model is replayed on.
//
// Both are append-only and have no foreign key: the record outlives a plan
// retention purges. A row is never deleted, and only the audit chain's own
// columns may be written after the insert; the chains (auditchain
// PlanEvents and PlanModelRecords) are installed by the audit chain
// migration, Bootstrap's last step. CREATE ... IF NOT EXISTS and catalog
// checks only; lock_timeout bounds the catalog locks on the first run.
const ddlPlanEvents = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.plan_event (
    id          bigserial PRIMARY KEY,
    plan_id     bigint NOT NULL,
    step_id     bigint,
    kind        text NOT NULL CHECK (kind IN (
                    'created', 'proposed', 'edited', 'approved', 'voided', 'rejected',
                    'step_started', 'step_executed', 'verdict', 'step_reverted', 'halted',
                    'resumed', 'completed', 'cancelled')),
    plan_hash   text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    actor       text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 200),
    detail      jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(detail) = 'object'),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS plan_event_plan ON sage.plan_event (plan_id, id);
CREATE TABLE IF NOT EXISTS sage.plan_model_record (
    id                 bigserial PRIMARY KEY,
    plan_id            bigint NOT NULL,
    pack_hash          text CHECK (pack_hash ~ '^[0-9a-f]{64}$'),
    model              text NOT NULL CHECK (char_length(model) BETWEEN 1 AND 200),
    prompt_version     text NOT NULL DEFAULT '' CHECK (char_length(prompt_version) <= 100),
    raw_output         text NOT NULL CHECK (octet_length(raw_output) <= 1048576),
    raw_output_sha256  text NOT NULL CHECK (raw_output_sha256 ~ '^[0-9a-f]{64}$'),
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS plan_model_record_plan ON sage.plan_model_record (plan_id, id);
` + ddlPlanAuditGuard

// ddlPlanAuditGuard refuses any delete of a plan event or model record,
// and any update except the audit chain writing its own columns into the
// row it just linked. A truncation is left to the chain, which records it
// and so fails the replay of every plan it covers.
const ddlPlanAuditGuard = `
CREATE OR REPLACE FUNCTION sage.plan_audit_append_only() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'pg_sage: %.% is append-only; row % cannot be deleted',
            TG_TABLE_SCHEMA, TG_TABLE_NAME, OLD.id USING ERRCODE = '55000';
    END IF;
    IF to_jsonb(NEW) - 'chain_seq' - 'chain_prev_hash' - 'chain_hash' IS DISTINCT FROM
       to_jsonb(OLD) - 'chain_seq' - 'chain_prev_hash' - 'chain_hash' THEN
        RAISE EXCEPTION 'pg_sage: %.% is append-only (row %)', TG_TABLE_SCHEMA,
            TG_TABLE_NAME, OLD.id USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $fn$;
DO $plan_audit$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'sage.plan_event'::regclass
                   AND tgname = 'plan_event_append_only') THEN
        CREATE TRIGGER plan_event_append_only BEFORE UPDATE OR DELETE ON sage.plan_event
            FOR EACH ROW EXECUTE FUNCTION sage.plan_audit_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger
                   WHERE tgrelid = 'sage.plan_model_record'::regclass
                   AND tgname = 'plan_model_record_append_only') THEN
        CREATE TRIGGER plan_model_record_append_only
            BEFORE UPDATE OR DELETE ON sage.plan_model_record
            FOR EACH ROW EXECUTE FUNCTION sage.plan_audit_append_only();
    END IF;
END
$plan_audit$;
`
