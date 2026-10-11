package schema

// ddlAuthAuditCreatedIndex lets the retention purge of sage.auth_audit
// (retention.auth_audit_days) find expired rows by index (perf gate A).
const ddlAuthAuditCreatedIndex = `
CREATE INDEX IF NOT EXISTS idx_auth_audit_created
    ON sage.auth_audit (created_at);
`
