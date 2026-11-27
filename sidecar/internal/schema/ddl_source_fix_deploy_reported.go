package schema

// ddlSourceFixDeployReportedAt adds sage.source_fix.deploy_reported_at
// (mcp-06): when pg_sage heard of the deploy. A deploy reported after its
// whole verify window had closed is then told apart from a live one.
// Reports filed before the column have it null (unknown). The catalog
// check first means a re-run takes no lock on sage.source_fix;
// lock_timeout bounds the wait of the first run.
const ddlSourceFixDeployReportedAt = `
SET LOCAL lock_timeout = '5s';
DO $source_fix_deploy_reported_at$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = 'sage.source_fix'::regclass
           AND attname = 'deploy_reported_at' AND NOT attisdropped
    ) THEN
        ALTER TABLE sage.source_fix ADD COLUMN IF NOT EXISTS deploy_reported_at timestamptz;
    END IF;
END
$source_fix_deploy_reported_at$;
`
