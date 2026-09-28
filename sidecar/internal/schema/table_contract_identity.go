package schema

// ddlTableContractIdentity makes a table contract unique per (database_id,
// schema, table) with NULL database_id treated as one identity. The
// original UNIQUE constraint does not deduplicate NULLs, so re-declaring a
// contract without a database id added a row each time. NULLS NOT DISTINCT
// needs PostgreSQL 15 and pg_sage supports 14, hence the COALESCE
// expression index (database ids are bigserial and start at 1, so 0 never
// names a real database). Existing duplicates are collapsed first, keeping
// the newest declaration (latest updated_at, then highest id). Idempotent:
// the DELETE finds nothing once rows are unique and the index is IF NOT
// EXISTS. The MCP upsert targets this index.
const ddlTableContractIdentity = `
DELETE FROM sage.table_contract older
USING sage.table_contract newer
WHERE COALESCE(older.database_id, 0) = COALESCE(newer.database_id, 0)
  AND older.schema_name = newer.schema_name
  AND older.table_name = newer.table_name
  AND (newer.updated_at, newer.id) > (older.updated_at, older.id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_table_contract_identity
    ON sage.table_contract ((COALESCE(database_id, 0)), schema_name, table_name);
`
