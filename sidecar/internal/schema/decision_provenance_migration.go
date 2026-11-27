package schema

// Decision provenance (policy-03). sage.decision.policy_id referenced
// sage.policy(id), but in fleet mode the policy lives in the control
// database, so the key could never hold, and nothing ever wrote the column
// (NULL in every row, so nothing is lost). The decision now names the
// policy it was made under by version and policy_doc_sha256, the SHA-256
// of the evaluated document's stored jsonb text, which is safe across
// databases.
//
// The column drop also drops the foreign key and idx_decision_policy. The
// CHECK is added NOT VALID: the column is new, so every existing row is
// NULL and a validating scan under the ALTER's exclusive lock would prove
// nothing; new and updated rows are checked. Each step checks the catalog
// first, so a re-run takes no lock; lock_timeout bounds the first run.
// Idempotent.
const ddlDecisionPolicyProvenance = `SET LOCAL lock_timeout = '5s';
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_attribute
                WHERE attrelid = 'sage.decision'::regclass AND NOT attisdropped
                  AND attname = 'policy_id') THEN
        ALTER TABLE sage.decision DROP COLUMN policy_id;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                    WHERE attrelid = 'sage.decision'::regclass AND NOT attisdropped
                      AND attname = 'policy_doc_sha256') THEN
        ALTER TABLE sage.decision ADD COLUMN policy_doc_sha256 text;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conrelid = 'sage.decision'::regclass
                      AND conname = 'decision_policy_doc_sha256_hex') THEN
        ALTER TABLE sage.decision ADD CONSTRAINT decision_policy_doc_sha256_hex
            CHECK (policy_doc_sha256 ~ '^[0-9a-f]{64}$') NOT VALID;
    END IF;
END $$;`
