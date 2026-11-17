package schema

// ddlCloneInstancesHolder gives every clone receipt its holder (the
// process using the clone), that holder's last heartbeat and the clone's
// cost label, written before the provider call (lab-05a). A reaper of
// another process reaps a clone whose holder stopped heartbeating before
// its TTL. Receipts written before this step have no holder: only their
// TTL ends them, as before.
const ddlCloneInstancesHolder = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.clone_instances
    ADD COLUMN IF NOT EXISTS holder_id text CHECK (length(holder_id) BETWEEN 1 AND 200),
    ADD COLUMN IF NOT EXISTS heartbeat_at timestamptz,
    ADD COLUMN IF NOT EXISTS estimate jsonb;
`
