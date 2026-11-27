package schema

// ddlAuditChainOpCheckValidated validates the widened link op check that
// ddlAuditChainMarkerOps (audit-03b) leaves NOT VALID on an install from
// before it. It is a step of its own, so it runs in its own transaction:
// VALIDATE CONSTRAINT takes only SHARE UPDATE EXCLUSIVE, which blocks no
// read or write while it scans, and never runs under the ACCESS EXCLUSIVE
// lock the replacement took. Without it an upgraded catalog differs from a
// fresh install's (the config-04b upgrade check). A fresh install, or a
// second bootstrap, finds the check validated and does nothing.
const ddlAuditChainOpCheckValidated = `
SET LOCAL lock_timeout = '5s';
DO $do$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = to_regclass('sage.audit_chain_link')
               AND conname = 'audit_chain_link_op_check' AND NOT convalidated) THEN
        ALTER TABLE sage.audit_chain_link VALIDATE CONSTRAINT audit_chain_link_op_check;
    END IF;
END $do$;
`
