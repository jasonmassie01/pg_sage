package schema

// ddlVerificationObservationRetry (executor-02) counts consecutive failed
// observations of a verification watch: a read error keeps the watch open
// (verdict extended, reason observation_retry) and only the hard deadline
// without a successful observation reverts it. The constant default makes
// ADD COLUMN a catalog-only change (PG11+); lock_timeout bounds the wait for
// its brief ACCESS EXCLUSIVE lock behind long readers.
const ddlVerificationObservationRetry = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.verification
    ADD COLUMN IF NOT EXISTS observation_failures integer NOT NULL DEFAULT 0;
`
