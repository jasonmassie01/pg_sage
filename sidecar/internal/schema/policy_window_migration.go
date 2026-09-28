package schema

// ddlPolicyWindowCronDuration upgrades stored policy documents to schema
// version 3. Up to version 2 a policy cron window matched only its minute;
// from version 3 a cron window is one hour wide unless it names a duration
// (D2, one window grammar with trust.maintenance_window). Each cron entry,
// the only five-token form the version 2 grammar had, is rewritten to
// "<cron> @1m" so the document keeps its exact meaning. Superseded and
// rejected rows stay as written, so policy history is not rewritten.
// Version 1 rows are left for ddlPolicyChangeClassSplit, which runs first
// and lifts them to version 2. Idempotent: only version 2 rows are touched.
const ddlPolicyWindowCronDuration = `
UPDATE sage.policy
SET doc = jsonb_set(doc, '{maintenance_windows}', (
    SELECT COALESCE(jsonb_agg(
        CASE WHEN cardinality(regexp_split_to_array(
                      btrim(w.entry, E' \t\r\n'), '\s+')) = 5
             THEN to_jsonb(btrim(w.entry, E' \t\r\n') || ' @1m')
             ELSE to_jsonb(w.entry)
        END ORDER BY w.ord), '[]'::jsonb)
    FROM jsonb_array_elements_text(doc->'maintenance_windows')
         WITH ORDINALITY AS w(entry, ord)))
WHERE schema_version = 2
  AND status IN ('active', 'ratified', 'proposed')
  AND jsonb_typeof(doc->'maintenance_windows') = 'array';

UPDATE sage.policy SET schema_version = 3
WHERE schema_version = 2 AND status IN ('active', 'ratified', 'proposed');

ALTER TABLE IF EXISTS sage.policy ALTER COLUMN schema_version SET DEFAULT 3;
`
