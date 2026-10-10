package schema

// Agent governance G1 upkeep (AGENTDB-SPEC §6.4, §6.6). Idempotent.
//
// sage.guard_principals.retired_by: the admin who retired the principal.
// The leader drops a retired principal's roles after
// agents.roles.retire_grace_days under that admin's approval; without one
// (retired before this column, or the admin was deleted) it reports and
// skips. A retired principal is immutable, so updated_at is when it
// retired. The partial indexes serve the user-delete cascade and the
// leader's due-for-drop and due-for-rotation scans.
const ddlGuardOps = `
ALTER TABLE sage.guard_principals ADD COLUMN IF NOT EXISTS retired_by integer
    REFERENCES sage.users(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS guard_principals_retired_by
    ON sage.guard_principals (retired_by) WHERE retired_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS guard_principals_retired_at
    ON sage.guard_principals (updated_at) WHERE status = 'retired';
CREATE INDEX IF NOT EXISTS guard_cluster_roles_rotated_at
    ON sage.guard_cluster_roles (rotated_at) WHERE status = 'active';
`
