package schema

// ddlIncidentOpenIdentity indexes open incidents by database and identity
// (dogfood lifeos-1): hydration backfills and merges legacy open
// incidents per identity, and the partial index keeps that bounded by
// the open set rather than the incident history. Registered migrations
// run before the incident lifecycle migration adds identity_key, so the
// column is ensured here too (the later ADD COLUMN IF NOT EXISTS is then
// a no-op). Idempotent.
const ddlIncidentOpenIdentity = `
ALTER TABLE sage.incidents ADD COLUMN IF NOT EXISTS identity_key TEXT;
CREATE INDEX IF NOT EXISTS idx_incidents_identity_open
    ON sage.incidents (database_name, identity_key)
    WHERE resolved_at IS NULL;
`
