package schema

// ddlAuthAuditSourceIP records the client address of every login and user
// change in sage.auth_audit (E1, CG-05). Existing rows keep an empty string.
const ddlAuthAuditSourceIP = `
ALTER TABLE sage.auth_audit
    ADD COLUMN IF NOT EXISTS source_ip TEXT NOT NULL DEFAULT '';
`
