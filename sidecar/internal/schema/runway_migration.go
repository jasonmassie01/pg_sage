package schema

// Sage SRE M6 runways: sage.runway_samples holds the series the runway
// monitor samples on the collector tick (XID and multixact counters, WAL
// position, retained WAL per slot, database and disk usage, sequences).
// A series is (kind, subject); epoch changes when a counter resets or the
// object is recreated, so a trend never spans two epochs. Values are
// finite: NaN and infinities are refused. Idempotent.
const ddlRunwaySamples = `
CREATE TABLE IF NOT EXISTS sage.runway_samples (
    id          bigserial PRIMARY KEY,
    kind        text NOT NULL CHECK (length(kind) BETWEEN 1 AND 32),
    subject     text NOT NULL CHECK (length(subject) BETWEEN 1 AND 256),
    epoch       text NOT NULL CHECK (length(epoch) BETWEEN 1 AND 64),
    sampled_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    value       double precision NOT NULL
        CHECK (value > '-Infinity' AND value < 'Infinity'),
    counter     double precision
        CHECK (counter > '-Infinity' AND counter < 'Infinity'),
    limit_value double precision
        CHECK (limit_value > '-Infinity' AND limit_value < 'Infinity')
);
CREATE INDEX IF NOT EXISTS runway_samples_series_idx
    ON sage.runway_samples (kind, subject, sampled_at DESC);
CREATE INDEX IF NOT EXISTS runway_samples_sampled_at_idx
    ON sage.runway_samples (sampled_at);
`
