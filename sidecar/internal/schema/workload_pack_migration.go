package schema

// ddlWorkloadPacks (evid-02a) stores built workload evidence packs: one
// row per database and content hash (content_sha256, the SHA-256 of the
// pack's sorted "name sha256" lines), with the pack's manifest and the
// gzip of its section files (at most 32 MB). database_id is NULL in the
// monitored placement and the meta-db record id in meta mode
// (history.store), so the unique key COALESCEs it. Only the local tier
// (the full pack, which never leaves the deployment) is stored. A row is
// never updated: a new build of the same window and data reuses it, and
// retention.workload_packs_days deletes old ones (on built_at).
//
// CREATE ... IF NOT EXISTS on a new table and its indexes; lock_timeout
// bounds the catalog locks on the first run.
const ddlWorkloadPacks = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.workload_packs (
    id                bigserial PRIMARY KEY,
    database_id       integer,
    window_from       timestamptz NOT NULL,
    window_to         timestamptz NOT NULL CHECK (window_to > window_from),
    format            text NOT NULL,
    content_sha256    text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    tier              text NOT NULL CHECK (tier = 'local'),
    bytes             bigint NOT NULL CHECK (bytes >= 0 AND bytes <= 33554432),
    built_at          timestamptz NOT NULL,
    built_by          text NOT NULL,
    manifest          jsonb NOT NULL,
    body              bytea NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS workload_packs_key
    ON sage.workload_packs (COALESCE(database_id, 0), content_sha256);
CREATE INDEX IF NOT EXISTS workload_packs_age ON sage.workload_packs (built_at);
`
