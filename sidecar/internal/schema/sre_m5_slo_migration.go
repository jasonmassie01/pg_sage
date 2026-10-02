package schema

// Sage SRE M5 SLOs and change events (AI-SRE-SPEC §8, §10; Codex §5):
//   - sre_service_slos: each SLO's definition and current error-budget
//     state per database (durable, read by the API, MCP and the autonomy
//     downgrade); sre_slo_transitions: its append-only state history.
//   - sre_sli_samples: cumulative counter samples of pushed app SLIs and
//     the database proxy SLIs, one per series and time (replays are
//     no-ops), bad never above eligible.
//   - sre_change_events: the change feed (signed external events and
//     pg_sage's own change sources), one row per source event id;
//     sre_change_feed_state: the feed's per-source cursors and snapshots.
//
// Like the other sre_* tables they live in the coordination database.
// Idempotent and additive.
const ddlSRESLOChangeEvents = `
CREATE TABLE IF NOT EXISTS sage.sre_service_slos (
    deployment_id   uuid NOT NULL,
    database_id     uuid NOT NULL,
    name            text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'),
    kind            text NOT NULL CHECK (kind IN ('app', 'proxy')),
    source          text NOT NULL CHECK (source IN ('prometheus', 'push', 'proxy')),
    target          double precision NOT NULL CHECK (target > 0 AND target < 1),
    definition_hash text NOT NULL CHECK (length(definition_hash) BETWEEN 1 AND 64),
    state           text NOT NULL CHECK (state IN ('ok', 'ticket', 'page', 'unknown')),
    fast_burning    boolean NOT NULL DEFAULT false,
    status          jsonb NOT NULL CHECK (jsonb_typeof(status) = 'object'
                        AND octet_length(status::text) <= 65536),
    evaluated_at    timestamptz NOT NULL,
    state_since     timestamptz NOT NULL,
    burn_started_at timestamptz,
    version         bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    PRIMARY KEY (deployment_id, database_id, name),
    FOREIGN KEY (deployment_id, database_id)
        REFERENCES sage.sre_database_bindings (deployment_id, database_id)
);

CREATE TABLE IF NOT EXISTS sage.sre_slo_transitions (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id uuid NOT NULL,
    database_id   uuid NOT NULL,
    name          text NOT NULL,
    from_state    text CHECK (from_state IN ('ok', 'ticket', 'page', 'unknown')),
    to_state      text NOT NULL CHECK (to_state IN ('ok', 'ticket', 'page', 'unknown')),
    at            timestamptz NOT NULL,
    FOREIGN KEY (deployment_id, database_id, name)
        REFERENCES sage.sre_service_slos (deployment_id, database_id, name)
        ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS sre_slo_transitions_by_slo
    ON sage.sre_slo_transitions (deployment_id, database_id, name, at DESC);

CREATE TABLE IF NOT EXISTS sage.sre_sli_samples (
    deployment_id uuid NOT NULL,
    slo_name      text NOT NULL CHECK (length(slo_name) BETWEEN 1 AND 64),
    series        text NOT NULL CHECK (length(series) BETWEEN 1 AND 128),
    observed_at   timestamptz NOT NULL,
    bad           double precision NOT NULL CHECK (bad >= 0),
    eligible      double precision NOT NULL CHECK (eligible >= 0),
    value         double precision,
    received_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, slo_name, series, observed_at),
    CHECK (bad <= eligible)
);
CREATE INDEX IF NOT EXISTS sre_sli_samples_time
    ON sage.sre_sli_samples (deployment_id, observed_at);

CREATE TABLE IF NOT EXISTS sage.sre_change_events (
    deployment_id    uuid NOT NULL,
    id               uuid NOT NULL,
    database_id      uuid,
    source           text NOT NULL CHECK (length(source) BETWEEN 1 AND 64),
    event_id         text NOT NULL CHECK (length(event_id) BETWEEN 1 AND 128),
    kind             text NOT NULL CHECK (kind IN ('deploy', 'migration', 'feature_flag',
        'config', 'ddl', 'sage_action', 'stats_reset', 'restart', 'failover',
        'extension', 'other')),
    service          text CHECK (length(service) <= 128),
    summary          text NOT NULL CHECK (length(summary) BETWEEN 1 AND 300),
    link             text CHECK (length(link) <= 512),
    objects          jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(objects) = 'array'),
    occurred_at      timestamptz NOT NULL,
    received_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    signature_status text NOT NULL CHECK (signature_status IN ('verified', 'internal')),
    change_hash      bytea NOT NULL CHECK (octet_length(change_hash) = 32),
    PRIMARY KEY (deployment_id, id),
    UNIQUE (deployment_id, source, event_id)
);
CREATE INDEX IF NOT EXISTS sre_change_events_time
    ON sage.sre_change_events (deployment_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS sage.sre_change_feed_state (
    deployment_id uuid NOT NULL,
    database_id   uuid NOT NULL,
    source        text NOT NULL CHECK (length(source) BETWEEN 1 AND 64),
    state         jsonb NOT NULL CHECK (jsonb_typeof(state) = 'object'),
    updated_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (deployment_id, database_id, source)
);
`
