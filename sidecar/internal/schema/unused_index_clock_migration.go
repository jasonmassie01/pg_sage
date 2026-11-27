package schema

// ddlUnusedIndexClock creates sage.unused_index_clock (rules-04): the
// analyzer's unused_index zero-scan clock, one row per index currently at
// zero scans, so a sidecar restart neither resolves an open finding nor
// restarts the window. indexrelid ties the clock to one object: an index
// dropped and recreated under the same name starts a new clock. In fleet
// mode each database's sage schema holds its own clock. Idempotent;
// lock_timeout bounds the first run's wait.
const ddlUnusedIndexClock = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.unused_index_clock (
    ident       text PRIMARY KEY,
    indexrelid  oid NOT NULL,
    zero_since  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
`
