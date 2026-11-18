package schema

// ddlReviewRuns is Sage Review's run record (review-01a): one row of
// sage.review_run per grade or review of a database, at most one running
// per database (review_run_one_running), and one row of sage.review_grade
// per dimension of a finished run. A killed sidecar's running row is
// failed by the reaper ("sidecar stopped during run"). They live in the
// graded database's own sage schema.
const ddlReviewRuns = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.review_run (
    id             bigserial PRIMARY KEY,
    database_name  text NOT NULL CHECK (length(database_name) BETWEEN 1 AND 200),
    kind           text NOT NULL CHECK (kind IN ('grade', 'review')),
    triggers       text[] NOT NULL DEFAULT '{}',
    rubric_version text NOT NULL CHECK (length(rubric_version) BETWEEN 1 AND 100),
    window_start   timestamptz,
    window_end     timestamptz,
    pack_hash      text,
    status         text NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'done', 'partial', 'failed')),
    overall_grade  text CHECK (overall_grade IN ('A', 'B', 'C', 'D', 'F')),
    overall_score  numeric(5,2) CHECK (overall_score BETWEEN 0 AND 100),
    coverage_pct   numeric(5,2) CHECK (coverage_pct BETWEEN 0 AND 100),
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    error          text,
    CHECK ((status = 'running') = (finished_at IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS review_run_one_running
    ON sage.review_run (database_name) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS review_run_database
    ON sage.review_run (database_name, id DESC);
CREATE TABLE IF NOT EXISTS sage.review_grade (
    run_id    bigint NOT NULL REFERENCES sage.review_run (id) ON DELETE CASCADE,
    ordinal   smallint NOT NULL CHECK (ordinal >= 0),
    dimension text NOT NULL CHECK (length(dimension) BETWEEN 1 AND 100),
    title     text NOT NULL DEFAULT '',
    letter    text CHECK (letter IN ('A', 'B', 'C', 'D', 'F')),
    score     numeric(5,2) NOT NULL CHECK (score BETWEEN 0 AND 100),
    coverage  numeric(5,2) NOT NULL CHECK (coverage BETWEEN 0 AND 100),
    checks    jsonb NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (run_id, dimension)
);
`
