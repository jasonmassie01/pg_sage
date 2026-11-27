package schema

// ddlUpgradeBriefs is the upgrade brief's history (drill-02a): one row per
// distinct brief of a database and target major, kept delta-only by the
// store (a brief whose hash equals the latest row's is not stored again),
// so a resolved blocker adds a row and keeps the old one. It lives in the
// briefed database's own sage schema, like its findings.
const ddlUpgradeBriefs = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.upgrade_briefs (
    id           bigserial PRIMARY KEY,
    database     text NOT NULL CHECK (length(database) BETWEEN 1 AND 200),
    target_major integer NOT NULL CHECK (target_major BETWEEN 1 AND 1000),
    built_at     timestamptz NOT NULL DEFAULT now(),
    brief        jsonb NOT NULL,
    hash         text NOT NULL CHECK (hash ~ '^[0-9a-f]{64}$')
);
CREATE INDEX IF NOT EXISTS upgrade_briefs_latest
    ON sage.upgrade_briefs (database, target_major, built_at DESC, id DESC);
`
