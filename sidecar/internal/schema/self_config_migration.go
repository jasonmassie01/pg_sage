package schema

// Self-configuration (roadmap phase 3, 2026-10-04), monitored database.
// sage.config_derived_setting holds one row per derived key: its status
// (default, derived, shadow, pinned, operator), the value in force, the
// promoted (active), shadow, pending-restart, pinned and operator values,
// the soak samples and the latest cited evidence and bounds.
// sage.config_derivation is the append-only derivation ledger: every
// shadow, promotion, hold, application, pin and unpin with the value, the
// previous value, the evidence, the bounds and the rule and version.
//
// Additive and idempotent: tables and the index are created when missing.
// The ledger is small (rows only on a change) and is not a retention
// target.
const ddlSelfConfig = `
CREATE TABLE IF NOT EXISTS sage.config_derived_setting (
    key             text PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_.]{0,127}$'),
    status          text NOT NULL CHECK (status IN
        ('default', 'derived', 'shadow', 'pinned', 'operator')),
    value           double precision NOT NULL,
    pending_value   double precision,
    active_value    double precision,
    shadow_value    double precision,
    shadow_since    timestamptz,
    shadow_reason   text NOT NULL DEFAULT '',
    shadow_outcome  text NOT NULL DEFAULT '' CHECK (shadow_outcome IN
        ('', 'not_worse', 'worse', 'insufficient')),
    samples         jsonb NOT NULL DEFAULT '[]'::jsonb,
    pinned_value    double precision,
    pinned_by       text NOT NULL DEFAULT '',
    pinned_at       timestamptz,
    operator_value  double precision,
    note            text NOT NULL DEFAULT '',
    rule            text NOT NULL,
    rule_version    integer NOT NULL CHECK (rule_version >= 1),
    evidence        jsonb NOT NULL DEFAULT '[]'::jsonb,
    bounds          jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT config_derived_setting_shadow CHECK (
        (shadow_value IS NULL) = (shadow_since IS NULL)),
    CONSTRAINT config_derived_setting_pin CHECK (
        (pinned_value IS NULL) = (pinned_at IS NULL)),
    CONSTRAINT config_derived_setting_status CHECK (
        (status <> 'pinned' OR pinned_value IS NOT NULL) AND
        (status <> 'shadow' OR shadow_value IS NOT NULL) AND
        (status <> 'operator' OR operator_value IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS sage.config_derivation (
    id              bigserial PRIMARY KEY,
    key             text NOT NULL,
    event           text NOT NULL CHECK (event IN ('shadow', 'promoted', 'applied',
        'held', 'cleared', 'operator_set', 'resumed', 'pinned', 'unpinned')),
    value           double precision,
    previous_value  double precision,
    evidence        jsonb NOT NULL DEFAULT '[]'::jsonb,
    bounds          jsonb NOT NULL DEFAULT '{}'::jsonb,
    rule            text NOT NULL,
    rule_version    integer NOT NULL CHECK (rule_version >= 1),
    reason          text NOT NULL DEFAULT '',
    actor           text NOT NULL DEFAULT 'pg_sage',
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS config_derivation_key_recent
    ON sage.config_derivation (key, created_at DESC, id DESC);
`
