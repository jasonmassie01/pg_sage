package schema

import "strings"

// footprint-06: the runtime no longer treats every untagged read of a
// statistics view (FROM pg_stat_statements, pg_stat_activity, ...) as
// pg_sage's own, since Datadog, pganalyze and pgwatch read the same views.
// Findings captured before pg_sage tagged its statements carry such a read
// and no tag; this migration resolves the open ones once, with the reason
// "pre-tagging pg_sage statement", and is the only place the retired
// signature remains.
//
// Once: only findings created before preTaggingCutoff qualify. Since the
// runtime dropped such findings before they were written, every open one
// predates the change; a monitor's finding written after the upgrade is
// later than the cutoff and never touched, and a rerun finds nothing open
// to resolve. A lock or statement timeout leaves the findings open for the
// next start instead of failing bootstrap (lock_timeout and
// statement_timeout bound the wait).

// preTaggingCatalogPattern is the retired runtime signature as a
// PostgreSQL regular expression (case-insensitive with ~*).
const preTaggingCatalogPattern = `\mfrom[[:space:]]+(pg_stat_statements|` +
	`pg_stat_user_tables|pg_stat_user_indexes|pg_stat_activity|pg_stat_database|` +
	`pg_stat_replication|pg_stat_bgwriter|pg_stat_checkpointer)\M`

// preTaggingCutoff is when the runtime signature was retired.
const preTaggingCutoff = "2026-11-09 00:00:00+00"

const ddlPreTaggingSelfFindingsTemplate = `
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '20s';
DO $pretagging_findings$
BEGIN
    UPDATE sage.findings f
       SET status = 'resolved', resolved_at = now(),
           detail = f.detail || jsonb_build_object('resolved_reason',
               'pre-tagging pg_sage statement')
     WHERE f.status = 'open'
       AND f.created_at < TIMESTAMPTZ '@cutoff@'
       AND jsonb_typeof(f.detail) = 'object'
       AND EXISTS (
           SELECT 1
             FROM unnest(ARRAY['query', 'query_text', 'normalized_query',
                               'sample_query', 'sql', 'statement']) AS k(key)
            WHERE coalesce(f.detail->>k.key, '') ~* '@pattern@'
              AND coalesce(f.detail->>k.key, '') NOT ILIKE '%/* pg_sage%');
EXCEPTION WHEN lock_not_available OR query_canceled THEN
    RAISE NOTICE 'pg_sage: pre-tagging findings left open until the next start: %',
        SQLERRM;
END
$pretagging_findings$;
`

// ddlPreTaggingSelfFindings is the migration statement.
var ddlPreTaggingSelfFindings = strings.NewReplacer(
	"@cutoff@", preTaggingCutoff, "@pattern@", preTaggingCatalogPattern,
).Replace(ddlPreTaggingSelfFindingsTemplate)
