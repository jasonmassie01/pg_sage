package schema

// Sage SRE M2 investigator state (AI-SRE-SPEC §10, Codex §5): the
// trigger's incident and subject, the persisted diagnosis summary, operator
// pinning, revisioned hypotheses with their supporting and contradicting
// evidence ids, an append-only event hash chain per investigation, and
// tombstones that retention leaves behind. Additive and idempotent.
const ddlSREInvestigator = `
ALTER TABLE sage.sre_investigations
    ADD COLUMN IF NOT EXISTS source_incident_id text,
    ADD COLUMN IF NOT EXISTS subject text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS pinned boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS summary jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS concluded_at timestamptz,
    ADD COLUMN IF NOT EXISTS evidence_purged_at timestamptz;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_investigations_m2_bounds'
                     AND conrelid = 'sage.sre_investigations'::regclass) THEN
        ALTER TABLE sage.sre_investigations ADD CONSTRAINT sre_investigations_m2_bounds
            CHECK (length(COALESCE(source_incident_id, '')) <= 256
               AND length(subject) <= 256
               AND jsonb_typeof(summary) = 'object'
               AND octet_length(summary::text) <= 65536);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS sage.sre_hypotheses (
    deployment_id     uuid NOT NULL,
    database_id       uuid NOT NULL,
    investigation_id  uuid NOT NULL,
    id                uuid NOT NULL,
    revision          integer NOT NULL CHECK (revision > 0),
    ordinal           integer NOT NULL CHECK (ordinal > 0),
    graph_version     text NOT NULL CHECK (length(graph_version) BETWEEN 1 AND 32),
    family            text NOT NULL CHECK (length(family) BETWEEN 1 AND 64),
    node_id           text NOT NULL CHECK (length(node_id) BETWEEN 1 AND 64),
    label             text NOT NULL CHECK (length(label) BETWEEN 1 AND 256),
    mechanism         text NOT NULL CHECK (length(mechanism) <= 1024),
    subject           text NOT NULL DEFAULT '' CHECK (length(subject) <= 256),
    status            text NOT NULL CHECK (status IN
        ('root_cause', 'contributing', 'unproven', 'ruled_out')),
    confidence        double precision NOT NULL CHECK (confidence BETWEEN 0 AND 1),
    supporting_ids    uuid[] NOT NULL DEFAULT '{}',
    contradicting_ids uuid[] NOT NULL DEFAULT '{}',
    support           jsonb NOT NULL CHECK (jsonb_typeof(support) = 'array'),
    contradict        jsonb NOT NULL CHECK (jsonb_typeof(contradict) = 'array'),
    refutation_probe  text NOT NULL CHECK (length(refutation_probe) BETWEEN 1 AND 64),
    operator_step     text NOT NULL DEFAULT '' CHECK (length(operator_step) <= 512),
    created_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id, investigation_id, id),
    UNIQUE (deployment_id, database_id, investigation_id, revision, ordinal),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK (octet_length(support::text) + octet_length(contradict::text) <= 65536),
    CHECK (status <> 'ruled_out' OR cardinality(contradicting_ids) > 0)
);

CREATE TABLE IF NOT EXISTS sage.sre_events (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    sequence         integer NOT NULL CHECK (sequence > 0),
    event_type       text NOT NULL CHECK (event_type IN ('created', 'claimed', 'step',
        'transition', 'concluded', 'pinned', 'unpinned', 'evidence_purged')),
    actor            text NOT NULL CHECK (length(actor) BETWEEN 1 AND 128),
    observed_at      timestamptz NOT NULL,
    payload          jsonb NOT NULL DEFAULT '{}'::jsonb,
    previous_hash    bytea,
    hash             bytea NOT NULL CHECK (octet_length(hash) = 32),
    PRIMARY KEY (deployment_id, database_id, investigation_id, sequence),
    FOREIGN KEY (deployment_id, database_id, investigation_id)
        REFERENCES sage.sre_investigations (deployment_id, database_id, id),
    CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (octet_length(payload::text) <= 16384),
    CHECK ((sequence = 1) = (previous_hash IS NULL)),
    CHECK (previous_hash IS NULL OR octet_length(previous_hash) = 32)
);

CREATE TABLE IF NOT EXISTS sage.sre_tombstones (
    deployment_id    uuid NOT NULL,
    database_id      uuid NOT NULL,
    investigation_id uuid NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('evidence', 'investigation')),
    deleted_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    reason           text NOT NULL CHECK (length(reason) BETWEEN 1 AND 128),
    row_count        integer NOT NULL CHECK (row_count >= 0),
    detail           jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(detail) = 'object'),
    PRIMARY KEY (deployment_id, database_id, investigation_id, kind)
);

-- Evidence and events are append-only: retention may delete them (and
-- leaves a tombstone), nothing may rewrite them.
CREATE OR REPLACE FUNCTION sage.sre_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'sage.% is append-only', TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END $$;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'sre_evidence_append_only'
                   AND tgrelid = 'sage.sre_evidence'::regclass) THEN
        CREATE TRIGGER sre_evidence_append_only BEFORE UPDATE ON sage.sre_evidence
            FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'sre_events_append_only'
                   AND tgrelid = 'sage.sre_events'::regclass) THEN
        CREATE TRIGGER sre_events_append_only BEFORE UPDATE ON sage.sre_events
            FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS sre_investigation_retention
    ON sage.sre_investigations (deployment_id, database_id, updated_at)
    WHERE NOT pinned;
`
