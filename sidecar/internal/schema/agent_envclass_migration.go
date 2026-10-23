package schema

// Agent governance G1 (spec §6.5, §6.7, §7). All idempotent.
//
// sage.guard_environment_labels binds each monitored database (by its
// sage.sre_database_bindings database_id) to branch, dev, stage or prod,
// with the identity tuple seen when the label was set. observed is the
// tuple last read by the reconciler; it is what the two-label check
// compares across databases. pending_* hold a widening label waiting for a
// second admin. Install-wide: it lives in the control database.
//
// sage.clone_instances is the clone receipt (§6.12): one atomic INSERT
// before the provider call, then the provider resource id once ready. A
// branch label is only verified by an active receipt.
//
// sage.facts gains column classes (fact type column_class, subject kind
// column, or table for a table-wide untrusted_input) keyed by (relid,
// attnum) so a rename keeps its class. The inline CHECKs are replaced once
// by versioned constraints.
const ddlAgentEnvClass = `
CREATE TABLE IF NOT EXISTS sage.guard_environment_labels (
    database_id    uuid PRIMARY KEY,
    label          text NOT NULL DEFAULT 'prod'
                   CHECK (label IN ('branch','dev','stage','prod')),
    identity       jsonb NOT NULL,
    verified       boolean NOT NULL DEFAULT false,
    set_by         text NOT NULL CHECK (length(set_by) BETWEEN 1 AND 200),
    set_at         timestamptz NOT NULL DEFAULT now(),
    observed       jsonb,
    observed_at    timestamptz,
    pending_label  text CHECK (pending_label IN ('branch','dev','stage','prod')),
    pending_by     text CHECK (length(pending_by) BETWEEN 1 AND 200),
    pending_at     timestamptz,
    CHECK ((pending_label IS NULL) = (pending_by IS NULL)
           AND (pending_label IS NULL) = (pending_at IS NULL))
);
CREATE INDEX IF NOT EXISTS guard_environment_labels_physical
    ON sage.guard_environment_labels ((observed->>'system_identifier'),
                                      (observed->>'db_oid'));

CREATE TABLE IF NOT EXISTS sage.clone_instances (
    id                 bigserial PRIMARY KEY,
    deployment_id      uuid NOT NULL,
    adapter            text NOT NULL,
    scope              text NOT NULL,
    ref                text,
    name               text NOT NULL,
    purpose            text NOT NULL CHECK (purpose IN ('rehearsal','sandbox','drill',
                           'gameday','bench','masked_parent')),
    principal_id       text,
    source_database_id uuid,
    masked             boolean NOT NULL DEFAULT false,
    status             text NOT NULL CHECK (status IN ('creating','create_uncertain','ready',
                           'destroying','destroyed','failed')),
    expires_at         timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (adapter, scope, name)
);
CREATE INDEX IF NOT EXISTS clone_instances_ready_name
    ON sage.clone_instances (name, created_at DESC) WHERE status = 'ready';

ALTER TABLE sage.facts ADD COLUMN IF NOT EXISTS subject_relid oid;
ALTER TABLE sage.facts ADD COLUMN IF NOT EXISTS subject_attnum smallint;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'facts_fact_type_v2'
                 AND conrelid = 'sage.facts'::regclass) THEN
    ALTER TABLE sage.facts DROP CONSTRAINT IF EXISTS facts_fact_type_check;
    ALTER TABLE sage.facts ADD CONSTRAINT facts_fact_type_v2 CHECK (fact_type IN (
        'owned_by_app_migrations', 'test_fixture', 'slot_consumer', 'append_only',
        'table_window', 'column_class'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'facts_subject_kind_v2'
                 AND conrelid = 'sage.facts'::regclass) THEN
    ALTER TABLE sage.facts DROP CONSTRAINT IF EXISTS facts_subject_kind_check;
    ALTER TABLE sage.facts ADD CONSTRAINT facts_subject_kind_v2 CHECK (subject_kind IN (
        'index', 'table', 'schema', 'slot', 'column'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'facts_column_subject_v1'
                 AND conrelid = 'sage.facts'::regclass) THEN
    ALTER TABLE sage.facts ADD CONSTRAINT facts_column_subject_v1 CHECK (
        fact_type <> 'column_class' OR (subject_relid IS NOT NULL AND (
          (subject_kind = 'column' AND subject_attnum > 0) OR
          (subject_kind = 'table' AND subject_attnum = 0))));
  END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS facts_column_class_uniq
    ON sage.facts (subject_relid, subject_attnum, fact_type) WHERE subject_kind = 'column';
CREATE INDEX IF NOT EXISTS facts_column_class_relid
    ON sage.facts (subject_relid) WHERE fact_type = 'column_class';
`
