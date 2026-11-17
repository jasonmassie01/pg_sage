package schema

// ddlReviewCapacityForecast (review-02f): each review's 30-day database
// size projection, one row per (database, review day), so the review 30
// days later measures how far off it was. A row is settled once: the
// capacity generator writes measured_bytes only while it is NULL.
const ddlReviewCapacityForecast = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_capacity_forecast (
    database_name   text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    made_on         date NOT NULL,
    target_on       date NOT NULL CHECK (target_on > made_on),
    projected_bytes float8 NOT NULL CHECK (projected_bytes >= 0),
    band_bytes      float8 NOT NULL CHECK (band_bytes >= 0),
    measured_bytes  float8 CHECK (measured_bytes IS NULL OR measured_bytes >= 0),
    error_pct       float8,
    measured_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (database_name, made_on),
    CHECK ((measured_bytes IS NULL) = (measured_at IS NULL))
);
`
