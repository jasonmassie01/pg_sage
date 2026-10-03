package schema

// Decision ledger dedupe (dogfood lifeos: the standing gate inserted a new
// sage.decision row on every evaluation, ~41,000 rows an hour). A
// non-execute verdict carries a fingerprint; repeats of an open
// fingerprint update one row, counting them in repeat_count and stamping
// last_seen_at. The unique index backs that upsert (INSERT ... ON
// CONFLICT), so it is created here, before any decision is written. Every
// existing row has no fingerprint, so the index starts empty; building it
// reads sage.decision once, under a lock that only pauses pg_sage's own
// decision writes during startup. Idempotent.
const ddlDecisionLedgerDedupe = `
ALTER TABLE sage.decision
    ADD COLUMN IF NOT EXISTS fingerprint text,
    ADD COLUMN IF NOT EXISTS repeat_count integer NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS last_seen_at timestamptz;
CREATE UNIQUE INDEX IF NOT EXISTS idx_decision_fingerprint
    ON sage.decision (fingerprint)
    WHERE fingerprint IS NOT NULL AND resolved_at IS NULL;
`
