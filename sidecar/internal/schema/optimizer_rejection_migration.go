package schema

// Optimizer what-if rejection memory. Idempotent.
//
// sage.optimizer_rejection remembers each LLM index candidate HypoPG measured
// and rejected: one row per database, table and normalized candidate shape
// (access method, ordered keys with opclass/collation/order, predicate,
// INCLUDE set; the index name is not part of it), keyed by shape_hash. The
// row keeps the measured improvement, the reason, and the workload it was
// measured on (target queryids with calls and mean time, the row
// estimate), so a later cycle can tell whether the workload changed
// materially before measuring the same idea again. A repeated measurement
// replaces the evidence and bumps measure_count.
const ddlOptimizerRejection = `
CREATE TABLE IF NOT EXISTS sage.optimizer_rejection (
    id                  bigserial PRIMARY KEY,
    database_name       text NOT NULL DEFAULT current_database(),
    schema_name         text NOT NULL CHECK (length(schema_name) BETWEEN 1 AND 128),
    table_name          text NOT NULL CHECK (length(table_name) BETWEEN 1 AND 128),
    shape_hash          text NOT NULL CHECK (length(shape_hash) = 64),
    method              text NOT NULL CHECK (length(method) BETWEEN 1 AND 64),
    key_cols            text[] NOT NULL CHECK (cardinality(key_cols) >= 1),
    predicate           text NOT NULL DEFAULT '',
    include_cols        text[] NOT NULL DEFAULT '{}',
    ddl                 text NOT NULL CHECK (length(ddl) <= 8192),
    improvement_pct     double precision NOT NULL,
    min_improvement_pct double precision NOT NULL,
    reason              text NOT NULL DEFAULT '' CHECK (length(reason) <= 1024),
    workload            jsonb NOT NULL DEFAULT '[]'::jsonb
                        CHECK (jsonb_typeof(workload) = 'array'),
    row_estimate        bigint NOT NULL DEFAULT 0,
    measure_count       integer NOT NULL DEFAULT 1 CHECK (measure_count >= 1),
    first_measured_at   timestamptz NOT NULL DEFAULT now(),
    measured_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (database_name, schema_name, table_name, shape_hash)
);
CREATE INDEX IF NOT EXISTS idx_optimizer_rejection_measured
    ON sage.optimizer_rejection (measured_at);
`
