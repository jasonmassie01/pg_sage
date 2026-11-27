package schema

// ddlPGAuditActionTagIndex finds the pgaudit records of a decision's tagged
// action session (audit-06): an action's evidence pack selects them by
// application = 'pg_sage action:<decision id>'. CREATE INDEX IF NOT EXISTS
// makes a re-run a no-op; lock_timeout bounds the first build's SHARE lock.
const ddlPGAuditActionTagIndex = `
SET LOCAL lock_timeout = '5s';
CREATE INDEX IF NOT EXISTS guard_pgaudit_events_action_tag
    ON sage.guard_pgaudit_events (application) WHERE correlated_by = 'action_tag';
`
