package schema

// Specialist contract revision 1.1.0. Idempotent.
//
// sage.specialist_requests.query_scope is the statement a caller scoped
// its investigation to ({"query_id", "query_hash", "applied"}), kept with
// its open/attach record so the result echoes it to that caller only.
// NULL for every v1 request.
const ddlSpecialistQueryScope = `
ALTER TABLE sage.specialist_requests ADD COLUMN IF NOT EXISTS query_scope jsonb
    CHECK (query_scope IS NULL OR (jsonb_typeof(query_scope) = 'object'
        AND pg_column_size(query_scope) <= 1024));
`
