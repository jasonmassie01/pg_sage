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

-- Only when the default is below 3: a second boot takes no lock (config-04).
DO $policy_default_3$
BEGIN
    IF to_regclass('sage.policy') IS NOT NULL AND COALESCE((
           SELECT NULLIF(regexp_replace(pg_get_expr(d.adbin, d.adrelid), '[^0-9]', '',
                                        'g'), '')::int
             FROM pg_attrdef d
             JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
            WHERE d.adrelid = to_regclass('sage.policy') AND a.attname = 'schema_version'),
           0) < 3 THEN
        ALTER TABLE sage.policy ALTER COLUMN schema_version SET DEFAULT 3;
    END IF;
END $policy_default_3$;
`
