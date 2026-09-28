package schema

// ddlAgentNativeIOAdmission stores the pg-side IO rates that earn load
// admission (D6) and the once-per-finding record of withheld admissions.
const ddlAgentNativeIOAdmission = `
CREATE TABLE IF NOT EXISTS sage.io_rate_sample (
    id                  bigserial PRIMARY KEY,
    database_name       text NOT NULL,
    sampled_at          timestamptz NOT NULL,
    interval_seconds    double precision NOT NULL,
    data_bytes_per_sec  double precision NOT NULL,
    wal_bytes_per_sec   double precision NOT NULL,
    source              text NOT NULL,
    CHECK (interval_seconds > 0),
    CHECK (data_bytes_per_sec >= 0),
    CHECK (wal_bytes_per_sec >= 0)
);
CREATE INDEX IF NOT EXISTS idx_io_rate_sample_database_time
    ON sage.io_rate_sample (database_name, sampled_at);

CREATE TABLE IF NOT EXISTS sage.admission_withheld (
    id              bigserial PRIMARY KEY,
    database_name   text NOT NULL,
    finding_key     text NOT NULL,
    reason          text NOT NULL,
    mode            text NOT NULL,
    detail          text NOT NULL DEFAULT '',
    evidence        jsonb NOT NULL DEFAULT '{}'::jsonb,
    decision_id     bigint,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    occurrences     integer NOT NULL DEFAULT 1,
    CHECK (occurrences > 0),
    UNIQUE (database_name, finding_key, reason)
);
CREATE INDEX IF NOT EXISTS idx_admission_withheld_recent
    ON sage.admission_withheld (database_name, last_seen_at DESC);
`
