package schema

// ddlBaselineSamples creates sage.baseline_samples (market-02): the
// hour-of-week workload baselines, one aggregated value (the mean of n
// cycles) per database, metric, key, week and bucket (0 is Monday 00:00
// in baseline.timezone). Retention keeps baseline.weeks + 1 weeks, so the
// table is bounded by max_query_keys x 168 x (weeks + 1) rows per
// database for statements, plus a few gauges.
const ddlBaselineSamples = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.baseline_samples (
    database    text NOT NULL,
    metric      text NOT NULL,
    key         text NOT NULL,
    how         text NOT NULL,
    week_start  date NOT NULL,
    bucket      smallint NOT NULL CONSTRAINT baseline_samples_bucket_check
                CHECK (bucket BETWEEN 0 AND 167),
    n           bigint NOT NULL,
    value       double precision NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database, metric, key, week_start, bucket)
);
CREATE INDEX IF NOT EXISTS baseline_samples_week_start_idx
    ON sage.baseline_samples (week_start);
`
