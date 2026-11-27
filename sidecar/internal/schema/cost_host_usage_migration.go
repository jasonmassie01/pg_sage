package schema

// ddlHostUsageHourly is the managed instance's host usage per hour
// (market-04): cloud telemetry rolls each poll into its resource's hour,
// so right-sizing and cost attribution have CPU and memory history after
// host samples leave memory. Keyed by provider resource, so the databases
// of one instance share a row. The writer keeps 35 days.
const ddlHostUsageHourly = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.host_usage_hourly (
    resource                text NOT NULL,
    hour                    timestamptz NOT NULL,
    instance_class          text NOT NULL DEFAULT '',
    vcpus                   integer NOT NULL DEFAULT 0 CHECK (vcpus >= 0),
    serverless              boolean NOT NULL DEFAULT false,
    memory_total_bytes      double precision,
    allocated_storage_bytes double precision,
    cpu_p50                 double precision,
    cpu_p95                 double precision,
    cpu_max                 double precision,
    mem_available_min       double precision,
    db_load_p95             double precision,
    connections_max         double precision,
    samples                 integer NOT NULL CHECK (samples > 0),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource, hour)
);
CREATE INDEX IF NOT EXISTS idx_host_usage_hourly_hour
    ON sage.host_usage_hourly (hour);
`
