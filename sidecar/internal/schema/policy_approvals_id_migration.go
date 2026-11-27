package schema

// ddlPolicyApprovalsID (audit-11) gives sage.policy_approvals the row id
// the audit chain keys its links on. The composite primary key (policy_id,
// approver) stays. Adding an identity column fills the existing rows (a
// small table rewrite under lock_timeout); the chain installed after the
// migration list then marks them as legacy. The catalog check keeps a
// second bootstrap from taking the table lock at all.
const ddlPolicyApprovalsID = `
SET LOCAL lock_timeout = '5s';
DO $do$
BEGIN
    IF to_regclass('sage.policy_approvals') IS NULL THEN
        RETURN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                   WHERE attrelid = 'sage.policy_approvals'::regclass
                   AND attname = 'id' AND NOT attisdropped) THEN
        ALTER TABLE sage.policy_approvals
            ADD COLUMN IF NOT EXISTS id bigint GENERATED ALWAYS AS IDENTITY UNIQUE;
    END IF;
END $do$;
`
