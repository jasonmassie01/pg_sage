package schema

// Five-minute time to value (roadmap phase 3). All idempotent.
//
// sage.first_look keeps each database's catalog-only first look: the
// findings with their cited catalog evidence, how each check went
// (degraded checks state why), the extensions pg_sage uses with their
// enablement steps, and an optional model summary. Only the newest few
// reports per database are kept.
//
// sage.onboarding keeps one row per monitored database: whether pg_sage
// found an existing install there or started a new one (an upgrade keeps
// its configured trust), when the first look finished and when the first
// finding appeared, with the time to first finding.
const ddlOnboarding = `
CREATE TABLE IF NOT EXISTS sage.first_look (
    id                   bigserial PRIMARY KEY,
    database_name        text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 128),
    provider             text NOT NULL DEFAULT '' CHECK (length(provider) <= 64),
    started_at           timestamptz NOT NULL,
    finished_at          timestamptz NOT NULL,
    duration_ms          bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    relations            integer NOT NULL DEFAULT 0 CHECK (relations >= 0),
    statement_timeout_ms integer NOT NULL DEFAULT 0 CHECK (statement_timeout_ms >= 0),
    items                jsonb NOT NULL DEFAULT '[]'::jsonb,
    checks               jsonb NOT NULL DEFAULT '[]'::jsonb,
    capabilities         jsonb NOT NULL DEFAULT '[]'::jsonb,
    summary              text NOT NULL DEFAULT '' CHECK (length(summary) <= 2000),
    summary_model        text NOT NULL DEFAULT '' CHECK (length(summary_model) <= 200),
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS first_look_database_newest
    ON sage.first_look (database_name, id DESC);
CREATE TABLE IF NOT EXISTS sage.onboarding (
    database_name        text PRIMARY KEY CHECK (length(database_name) BETWEEN 1 AND 128),
    install_kind         text NOT NULL CHECK (install_kind IN ('new', 'existing')),
    installed_at         timestamptz NOT NULL DEFAULT now(),
    first_look_at        timestamptz,
    first_finding_at     timestamptz,
    first_finding_source text NOT NULL DEFAULT ''
                         CHECK (length(first_finding_source) <= 64),
    ttff_ms              bigint CHECK (ttff_ms >= 0)
);
`
