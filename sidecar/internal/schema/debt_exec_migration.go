package schema

// ddlDebtExec adds what the single execution pipeline needs for typed
// leases, serialize_mode=queue and retention deletes run through Apply.
//
//   - sage.change_lease records the exact object a lease covers (kind, OID,
//     canonical name) and the actor holding it, so a refusal can name the
//     holder.
//   - sage.lease_queue is the durable FIFO queue of requests waiting for a
//     leased object (serialize_mode=queue). At most one waiting entry per
//     request: a restarted sidecar resumes it instead of queueing again.
//   - sage.retention_run links an applied batch to the action that ran it
//     and to the reviewed dry run that authorized it.
//
// Additive and idempotent; no existing row changes.
const ddlDebtExec = `
ALTER TABLE sage.change_lease
    ADD COLUMN IF NOT EXISTS actor text,
    ADD COLUMN IF NOT EXISTS object_type text,
    ADD COLUMN IF NOT EXISTS object_oid oid,
    ADD COLUMN IF NOT EXISTS object_name text;

ALTER TABLE sage.retention_run
    ADD COLUMN IF NOT EXISTS action_id bigint,
    ADD COLUMN IF NOT EXISTS dry_run_id bigint;

CREATE TABLE IF NOT EXISTS sage.lease_queue (
    id              bigserial PRIMARY KEY,
    database_id     bigint,
    request_key     text NOT NULL,
    object_keys     text[] NOT NULL,
    kind            text NOT NULL,
    actor           text NOT NULL,
    intent          text NOT NULL,
    decision_id     bigint,
    instance        text NOT NULL,
    state           text NOT NULL DEFAULT 'waiting',
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    heartbeat_at    timestamptz NOT NULL DEFAULT now(),
    deadline_at     timestamptz NOT NULL,
    resolved_at     timestamptz,
    resumed_count   integer NOT NULL DEFAULT 0,
    CHECK (state IN ('waiting', 'granted', 'timed_out', 'cancelled'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_lease_queue_waiting_request
    ON sage.lease_queue ((COALESCE(database_id, 0)), request_key)
    WHERE state = 'waiting';
CREATE INDEX IF NOT EXISTS idx_lease_queue_waiting_keys
    ON sage.lease_queue USING gin (object_keys)
    WHERE state = 'waiting';
CREATE INDEX IF NOT EXISTS idx_retention_run_dry_run
    ON sage.retention_run (dry_run_id)
    WHERE dry_run_id IS NOT NULL;
`
