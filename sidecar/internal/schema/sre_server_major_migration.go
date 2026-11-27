package schema

// earned-07 (2026-11-11): a level records the PostgreSQL major of the
// monitored server it was granted on (server_major). A self-initiated L3
// granted on one major is capped at L2 on another until it re-validates.
// Nullable: rows from before the column are stamped with the major the
// sidecar next observes, without a cap. Additive and idempotent (ADD
// COLUMN IF NOT EXISTS, a re-run takes no lock past its catalog check);
// lock_timeout bounds the first run's wait for the table. Old binaries
// never read or write the column.
const ddlSREAutonomyServerMajor = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.sre_family_autonomy
    ADD COLUMN IF NOT EXISTS server_major smallint
        CHECK (server_major BETWEEN 10 AND 999);
`
