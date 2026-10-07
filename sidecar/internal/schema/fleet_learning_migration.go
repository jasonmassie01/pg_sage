package schema

// ddlFleetLearning creates the fleet learning tables (roadmap phase 3).
// They are used in the control database (meta DB, else the fleet's
// primary), keyed by fleet scope; like every table they exist everywhere.
//
//   - fleet_fingerprint: per database, hashes of table, index and query
//     shapes (no names, data or literals; table_labels only when the
//     operator opts in) and its sharing boundary.
//   - fleet_outcome_digest: per database, verified outcome counts by action
//     class and table shape (” = the class over every table).
//   - fleet_leader_lease: one lease per fleet scope; epoch is the fencing
//     token, bumped on every change of holder.
const ddlFleetLearning = `
CREATE TABLE IF NOT EXISTS sage.fleet_fingerprint (
    fleet_scope   text NOT NULL,
    database_name text NOT NULL,
    boundary      text NOT NULL DEFAULT '',
    table_shapes  text[] NOT NULL DEFAULT '{}',
    index_shapes  text[] NOT NULL DEFAULT '{}',
    query_shapes  text[] NOT NULL DEFAULT '{}',
    table_labels  jsonb,
    computed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (fleet_scope, database_name)
);
CREATE TABLE IF NOT EXISTS sage.fleet_outcome_digest (
    fleet_scope   text NOT NULL,
    database_name text NOT NULL,
    action_class  text NOT NULL,
    shape         text NOT NULL DEFAULT '',
    improved      integer NOT NULL DEFAULT 0 CHECK (improved >= 0),
    neutral       integer NOT NULL DEFAULT 0 CHECK (neutral >= 0),
    regressed     integer NOT NULL DEFAULT 0 CHECK (regressed >= 0),
    computed_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (fleet_scope, database_name, action_class, shape)
);
CREATE TABLE IF NOT EXISTS sage.fleet_leader_lease (
    scope       text PRIMARY KEY,
    holder      text NOT NULL,
    epoch       bigint NOT NULL CHECK (epoch > 0),
    acquired_at timestamptz NOT NULL DEFAULT now(),
    renewed_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);`
