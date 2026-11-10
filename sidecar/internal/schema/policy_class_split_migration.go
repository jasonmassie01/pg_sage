package schema

// ddlPolicyChangeClassSplit upgrades stored policy documents to schema
// version 2. Version 1 predates G4-B18, when backend signals, query hints and
// schema changes were authorized as change class "index". The upgrade keeps
// each document's effective permission: the split classes are granted (and
// require approval) exactly where "index" was. Superseded and rejected rows
// stay as written, so policy history is not rewritten. Rows written from now
// on default to version 2 and are never widened. Idempotent: only rows still
// at version 1 are touched.
const ddlPolicyChangeClassSplit = `
UPDATE sage.policy
SET doc = jsonb_set(doc, '{approval_required_classes}',
    (doc->'approval_required_classes') || (
        SELECT COALESCE(jsonb_agg(split.class), '[]'::jsonb)
        FROM unnest(ARRAY['backend_signal', 'query_hint', 'schema_change']) AS split(class)
        WHERE NOT ((doc->'approval_required_classes') ? split.class)))
WHERE schema_version < 2
  AND status IN ('active', 'ratified', 'proposed')
  AND jsonb_typeof(doc->'approval_required_classes') = 'array'
  AND (doc->'approval_required_classes') ? 'index';

UPDATE sage.policy
SET doc = jsonb_set(doc, '{allowed_change_classes}',
    (doc->'allowed_change_classes') || (
        SELECT COALESCE(jsonb_agg(split.class), '[]'::jsonb)
        FROM unnest(ARRAY['backend_signal', 'query_hint', 'schema_change']) AS split(class)
        WHERE NOT ((doc->'allowed_change_classes') ? split.class)))
WHERE schema_version < 2
  AND status IN ('active', 'ratified', 'proposed')
  AND jsonb_typeof(doc->'allowed_change_classes') = 'array'
  AND (doc->'allowed_change_classes') ? 'index';

UPDATE sage.policy SET schema_version = 2
WHERE schema_version < 2 AND status IN ('active', 'ratified', 'proposed');

-- Only when the default is below 2: a second boot takes no lock (config-04).
DO $policy_default_2$
BEGIN
    IF to_regclass('sage.policy') IS NOT NULL AND COALESCE((
           SELECT NULLIF(regexp_replace(pg_get_expr(d.adbin, d.adrelid), '[^0-9]', '',
                                        'g'), '')::int
             FROM pg_attrdef d
             JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
            WHERE d.adrelid = to_regclass('sage.policy') AND a.attname = 'schema_version'),
           0) < 2 THEN
        ALTER TABLE sage.policy ALTER COLUMN schema_version SET DEFAULT 2;
    END IF;
END $policy_default_2$;
`
