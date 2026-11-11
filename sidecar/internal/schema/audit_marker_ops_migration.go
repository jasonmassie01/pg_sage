package schema

// ddlAuditChainMarkerOps lets the link table hold signed checkpoint (C)
// and prune (P) links (audit-03b), and indexes them so the newest of each
// is found without walking a chain. A fresh install gets both from the
// link table's own DDL (auditchain.MigrationSQL, which runs later), so
// this step only widens an install from before: the old four-op check is
// replaced NOT VALID (every existing row already passes it, and no scan of
// the table runs under the lock). The catalog checks keep a second
// bootstrap from taking the table lock at all.
const ddlAuditChainMarkerOps = `
SET LOCAL lock_timeout = '5s';
DO $do$
BEGIN
    IF to_regclass('sage.audit_chain_link') IS NULL THEN
        RETURN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conrelid = 'sage.audit_chain_link'::regclass
                   AND conname = 'audit_chain_link_op_check'
                   AND pg_get_constraintdef(oid) LIKE '%''P''%') THEN
        ALTER TABLE sage.audit_chain_link
            DROP CONSTRAINT IF EXISTS audit_chain_link_op_check,
            ADD CONSTRAINT audit_chain_link_op_check
                CHECK (op IN ('I','U','D','T','C','P')) NOT VALID;
    END IF;
    IF to_regclass('sage.audit_chain_link_marker') IS NULL THEN
        CREATE INDEX IF NOT EXISTS audit_chain_link_marker
            ON sage.audit_chain_link (chain, seq) WHERE op IN ('C','P');
    END IF;
END $do$;
`
