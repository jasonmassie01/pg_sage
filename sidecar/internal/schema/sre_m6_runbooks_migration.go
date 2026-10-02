package schema

// Sage SRE M6 typed runbooks and incident memory (AI-SRE-SPEC §7.1, §10
// sre_runbooks). Runbooks are scoped to one database, like every SRE row.
// A version's content is immutable (a trigger refuses updates of it and
// any change once signed); a signature binds the version's own content
// hash. Run history and operator outcomes belong to their investigation
// and go with it when retention deletes it. Additive and idempotent.
const ddlSRERunbooks = `
CREATE TABLE IF NOT EXISTS sage.sre_runbooks (
    deployment_id  uuid NOT NULL,
    database_id    uuid NOT NULL,
    id             uuid NOT NULL,
    latest_version integer NOT NULL DEFAULT 1 CHECK (latest_version > 0),
    created_by     text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 128),
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    retired_by     text CHECK (length(retired_by) BETWEEN 1 AND 128),
    retired_at     timestamptz,
    PRIMARY KEY (deployment_id, database_id, id),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id),
    CHECK ((retired_at IS NULL) = (retired_by IS NULL))
);

CREATE TABLE IF NOT EXISTS sage.sre_runbook_versions (
    deployment_id uuid NOT NULL,
    database_id   uuid NOT NULL,
    runbook_id    uuid NOT NULL,
    version       integer NOT NULL CHECK (version > 0),
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    definition    jsonb NOT NULL CHECK (jsonb_typeof(definition) = 'object'
                      AND octet_length(definition::text) <= 32768),
    content_hash  bytea NOT NULL CHECK (octet_length(content_hash) = 32),
    source        text NOT NULL CHECK (source IN ('manual', 'compiled')),
    source_text   text CHECK (length(source_text) <= 16000),
    compiled_by   text CHECK (length(compiled_by) <= 128),
    created_by    text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 128),
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    signed_by     text CHECK (length(signed_by) BETWEEN 1 AND 128),
    signer_role   text CHECK (signer_role = 'admin'),
    signed_hash   bytea,
    signed_at     timestamptz,
    PRIMARY KEY (deployment_id, database_id, runbook_id, version),
    FOREIGN KEY (deployment_id, database_id, runbook_id)
        REFERENCES sage.sre_runbooks (deployment_id, database_id, id),
    CHECK ((signed_at IS NULL) = (signed_by IS NULL)),
    CHECK ((signed_at IS NULL) = (signed_hash IS NULL)),
    CHECK ((signed_at IS NULL) = (signer_role IS NULL)),
    CHECK (signed_hash IS NULL OR signed_hash = content_hash)
);

CREATE OR REPLACE FUNCTION sage.sre_runbook_version_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'sage.sre_runbook_versions is append-only: a runbook version '
            'cannot be deleted' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.signed_at IS NOT NULL THEN
        RAISE EXCEPTION 'a signed runbook version cannot change'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF (NEW.deployment_id, NEW.database_id, NEW.runbook_id, NEW.version, NEW.name,
        NEW.definition, NEW.content_hash, NEW.source, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.deployment_id, OLD.database_id, OLD.runbook_id, OLD.version, OLD.name,
        OLD.definition, OLD.content_hash, OLD.source, OLD.created_by, OLD.created_at)
       OR NEW.source_text IS DISTINCT FROM OLD.source_text
       OR NEW.compiled_by IS DISTINCT FROM OLD.compiled_by THEN
        RAISE EXCEPTION 'a runbook version''s content is immutable'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'sre_runbook_versions_guard'
                   AND tgrelid = 'sage.sre_runbook_versions'::regclass) THEN
        CREATE TRIGGER sre_runbook_versions_guard
            BEFORE UPDATE OR DELETE ON sage.sre_runbook_versions
            FOR EACH ROW EXECUTE FUNCTION sage.sre_runbook_version_guard();
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS sage.sre_runbook_runs (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    runbook_id       uuid NOT NULL,
    version          integer NOT NULL CHECK (version > 0),
    content_hash     bytea NOT NULL CHECK (octet_length(content_hash) = 32),
    outcome          text NOT NULL CHECK (outcome IN
        ('completed', 'abstained', 'probe_budget_exhausted', 'no_time')),
    path             jsonb NOT NULL CHECK (jsonb_typeof(path) = 'array'
                         AND octet_length(path::text) <= 4096),
    proposal         jsonb CHECK (proposal IS NULL OR (jsonb_typeof(proposal) = 'object'
                         AND octet_length(proposal::text) <= 4096)),
    reason           text NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
    probes           integer NOT NULL CHECK (probes BETWEEN 0 AND 12),
    created_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id, investigation_id, runbook_id, version),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id)
        ON DELETE CASCADE,
    FOREIGN KEY (deployment_id, database_id, runbook_id, version)
        REFERENCES sage.sre_runbook_versions (deployment_id, database_id, runbook_id, version)
);
CREATE INDEX IF NOT EXISTS sre_runbook_runs_by_runbook
    ON sage.sre_runbook_runs (deployment_id, database_id, runbook_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sage.sre_investigation_outcomes (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id               uuid NOT NULL,
    verdict          text NOT NULL CHECK (verdict IN ('confirmed', 'refuted')),
    actual_node      text CHECK (length(actual_node) BETWEEN 1 AND 64),
    actor            text NOT NULL CHECK (length(actor) BETWEEN 1 AND 128),
    recorded_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id, investigation_id, id),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id)
        ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS sre_outcomes_by_time
    ON sage.sre_investigation_outcomes
        (deployment_id, database_id, investigation_id, recorded_at DESC);

CREATE INDEX IF NOT EXISTS sre_investigation_memory
    ON sage.sre_investigations (deployment_id, database_id, trigger_kind, concluded_at)
    WHERE state IN ('concluded', 'inconclusive');
`
