package schema

// ddlRetentionColumnDeclaration (decision D5) adds the owner-declared
// retention column to table contracts and the identity a retention dry run
// is bound to. Existing contracts are deliberately NOT backfilled: pg_sage
// no longer infers the column, so a legacy contract parks (deletes nothing)
// until its owner re-declares it with retention.column. No CHECK constraint:
// legacy rows must still load; the declaration path enforces the column.
// Idempotent: every column is added only if missing.
const ddlRetentionColumnDeclaration = `
ALTER TABLE sage.table_contract
    ADD COLUMN IF NOT EXISTS retention_column text;
ALTER TABLE sage.retention_run
    ADD COLUMN IF NOT EXISTS relation_oid oid,
    ADD COLUMN IF NOT EXISTS column_attnum smallint,
    ADD COLUMN IF NOT EXISTS column_type text,
    ADD COLUMN IF NOT EXISTS contract_id bigint,
    ADD COLUMN IF NOT EXISTS contract_updated_at timestamptz;
`
