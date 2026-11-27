package schema

// observability-08: sage.mcp_tokens.scopes also admits the metrics scope,
// which authorizes a Prometheus scrape of /metrics and nothing else. The
// CHECK repeats the store's rule: metrics is a token's only scope, on an
// operator token; every other token keeps read, propose and approve with
// read required. The inline CHECK of ddlMCPv2 (auto-named
// mcp_tokens_scopes_check) is replaced once; the table is tiny, so the
// validating ADD holds its lock briefly, under lock_timeout.
const ddlMCPTokensMetricsScope = `
SET LOCAL lock_timeout = '5s';
DO $mcp_tokens_metrics$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'sage.mcp_tokens'::regclass
           AND conname = 'mcp_tokens_scopes_v2_check'
    ) THEN
        ALTER TABLE sage.mcp_tokens DROP CONSTRAINT IF EXISTS mcp_tokens_scopes_check;
        ALTER TABLE sage.mcp_tokens ADD CONSTRAINT mcp_tokens_scopes_v2_check CHECK (
            (scopes <@ ARRAY['read', 'propose', 'approve'] AND 'read' = ANY (scopes))
            OR (scopes = ARRAY['metrics'] AND kind = 'operator'));
    END IF;
END
$mcp_tokens_metrics$;
`
