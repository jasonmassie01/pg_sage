package schema

const ddlAgentNativeFeatureState = `
CREATE TABLE IF NOT EXISTS sage.slot_consumer_registry (
    slot_name           text PRIMARY KEY,
    owner_tag           text NOT NULL,
    consumer_identity   text,
    registered          boolean NOT NULL DEFAULT true,
    last_confirmed_at   timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sage.table_contract (
    id                  bigserial PRIMARY KEY,
    database_id         bigint,
    schema_name         text NOT NULL,
    table_name          text NOT NULL,
    append_only         boolean NOT NULL DEFAULT false,
    retention_interval interval,
    expected_pk         text,
    exemptions          jsonb NOT NULL DEFAULT '[]'::jsonb,
    declared_by         text NOT NULL,
    evidence_id         text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (database_id, schema_name, table_name),
    UNIQUE (evidence_id)
);

CREATE TABLE IF NOT EXISTS sage.migration_run (
    id                      bigserial PRIMARY KEY,
    database_id             bigint,
    evidence_id             text NOT NULL UNIQUE,
    phase                   text NOT NULL,
    source_sql_hash         text NOT NULL,
    clone_id                text,
    clone_created_from      timestamptz,
    measurement             jsonb NOT NULL DEFAULT '{}'::jsonb,
    verdict                 text NOT NULL,
    contract_not_before     timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (phase IN ('plan', 'rehearse', 'expand', 'verify', 'contract')),
    CHECK (verdict IN ('pending', 'recommend_only', 'parked', 'promote', 'failed'))
);

CREATE TABLE IF NOT EXISTS sage.retention_run (
    id                  bigserial PRIMARY KEY,
    database_id         bigint,
    schema_name         text NOT NULL,
    table_name          text NOT NULL,
    retention_column    text NOT NULL,
    cutoff_at           timestamptz NOT NULL,
    candidate_rows      bigint NOT NULL DEFAULT 0,
    deleted_rows        bigint NOT NULL DEFAULT 0,
    disposition         text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (disposition IN ('dry_run', 'applied', 'failed'))
);
CREATE INDEX IF NOT EXISTS idx_retention_run_target
    ON sage.retention_run (schema_name, table_name, created_at DESC);

CREATE TABLE IF NOT EXISTS sage.rollout_run (
    id                          bigserial PRIMARY KEY,
    evidence_id                 text NOT NULL UNIQUE,
    source_instance             text NOT NULL,
    prior_evidence_id           text NOT NULL,
    policy                      jsonb NOT NULL,
    state                       text NOT NULL,
    aggregate_regression_pct    numeric,
    applied_instances           integer NOT NULL DEFAULT 0,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CHECK (state IN ('canary', 'expanding', 'halted', 'complete', 'failed'))
);
`
