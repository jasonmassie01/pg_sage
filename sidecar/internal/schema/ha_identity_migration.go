package schema

// sage.ha_identity (Sage SRE follow-ups B): each HA monitor's last
// observed node identity (role, timeline, system identifier, postmaster
// start) and when the node last changed, so a failover that happened
// while pg_sage was down opens the earned-autonomy failover cooldown at
// startup. One row per monitor (a stable key of the monitored database),
// in the control database. Unknown fields are NULL. Idempotent.
const ddlHAIdentity = `
CREATE TABLE IF NOT EXISTS sage.ha_identity (
    monitor_key       text PRIMARY KEY CHECK (length(monitor_key) BETWEEN 1 AND 200),
    role              text NOT NULL CHECK (role IN ('primary', 'replica')),
    timeline_id       bigint CHECK (timeline_id > 0),
    system_identifier text CHECK (length(system_identifier) BETWEEN 1 AND 32),
    server_started_at timestamptz,
    last_change_at    timestamptz,
    observed_at       timestamptz NOT NULL
);
`
